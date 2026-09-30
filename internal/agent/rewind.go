package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// NewEventID returns a new event id, for a store that records an event of
// its own, such as record.repaired.
func NewEventID() string { return newID() }

// EvCheckpoint records a file's content just before the agent changed it,
// kept in the record's blob store by its sha256, so the change can be
// undone or rewound after the process that made it has gone.
const EvCheckpoint EventType = "checkpoint.saved"

// CheckpointSaved is the payload of checkpoint.saved.
type CheckpointSaved struct {
	Path string `json:"path"`
	// SHA256 names the blob holding the content; "" when the file did not exist.
	SHA256 string `json:"sha256,omitempty"`
	Turn   int    `json:"turn"`
}

// BlobRefs lists the blobs a session's events name: its checkpoints and the
// contents its restores replaced and wrote.
func BlobRefs(events []Event) []string {
	var out []string
	for _, ev := range events {
		switch ev.Type {
		case EvCheckpoint:
			var c CheckpointSaved
			if json.Unmarshal(ev.Payload, &c) == nil && c.SHA256 != "" {
				out = append(out, c.SHA256)
			}
		case EvFileRestored:
			var r FileRestored
			if json.Unmarshal(ev.Payload, &r) == nil {
				for _, h := range []string{r.BeforeSHA256, r.AfterSHA256} {
					if h != "" {
						out = append(out, h)
					}
				}
			}
		}
	}
	return out
}

// RewindPoint is a prompt the conversation can be taken back to: the state
// just before it was sent.
type RewindPoint struct {
	Seq  int64
	Text string
}

// RewindPoints are the person's prompts in the conversation as it stands,
// oldest first.
func RewindPoints(events []Event) []RewindPoint {
	var out []RewindPoint
	for _, ev := range Live(events) {
		if ev.Type != EvUserMessage {
			continue
		}
		var m Message
		if json.Unmarshal(ev.Payload, &m) == nil {
			out = append(out, RewindPoint{Seq: ev.Seq, Text: m.Text})
		}
	}
	return out
}

// ForkBefore takes the conversation back to just before step seq, a prompt:
// a fork through the last step before it, recorded as conversation.forked.
// Before the first prompt that is a fork at 0, an empty conversation in the
// same session; the loop is never dropped. It returns how many messages
// were kept.
func (l *Loop) ForkBefore(events []Event, seq int64) (int, error) {
	var through int64
	said := false
	for _, ev := range Live(events) {
		if ev.Seq >= seq {
			break
		}
		// The last step before the prompt, so the fork keeps the end of the
		// turn before it.
		through = ev.Seq
		switch ev.Type {
		case EvUserMessage, EvAgentMessage, EvObservation, EvSubagentNotice:
			said = true
		}
	}
	if said {
		return l.ForkTo(events, through)
	}
	l.runMu.Lock()
	defer l.runMu.Unlock()
	if _, err := l.Recorder.Record(EvForked, ActorUser, Trusted, Forked{ThroughSeq: 0}); err != nil {
		return 0, err
	}
	l.Session.ResetScoped()
	l.messages = nil
	return 0, nil
}

// RestoreFile is how a rewind puts one file back: path to data, or removed
// when existed is false. It is the session's, confined to its roots.
type RestoreFile func(path string, data []byte, existed bool) error

// BlobPutter keeps a file's content by its hash, for a restore to be undone.
type BlobPutter interface {
	Put(data []byte) (string, error)
}

// RestoreCheckpoints puts files back as the checkpoints say, as the person's
// action: each is put to the policy as a write, a denial recorded and the
// file left alone, and each restore recorded as file.restored with the
// content's hash before and after. It returns a line per file.
func (l *Loop) RestoreCheckpoints(cps []Checkpoint, current func(path string) ([]byte, bool), restore RestoreFile, blobs BlobPutter) ([]string, error) {
	var done []string
	var failed []string
	for _, cp := range cps {
		args, _ := json.Marshal(map[string]string{"path": cp.Path})
		if d := l.Policy.Evaluate("write", true, args); d.Decision == policy.Deny {
			_ = l.ManualRefused("write", "restore-"+newID(), args, d)
			failed = append(failed, fmt.Sprintf("%s: refused: %s", cp.Path, d.Reason))
			continue
		}
		data, err := cp.content()
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", cp.Path, err))
			continue
		}
		before := ""
		if now, ok := current(cp.Path); ok {
			before = hashOf(now)
			if blobs != nil {
				_, _ = blobs.Put(now) // so this restore can be undone in turn
			}
		}
		if err := restore(cp.Path, data, cp.Existed); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", cp.Path, err))
			continue
		}
		after := ""
		if cp.Existed {
			after = hashOf(data)
		}
		if _, err := l.Recorder.Record(EvFileRestored, ActorUser, Trusted, FileRestored{
			Path: cp.Path, BeforeSHA256: before, AfterSHA256: after,
			Checkpoint: strconv.FormatInt(cp.Seq, 10), By: "user",
		}); err != nil {
			return done, err
		}
		verb := "restored "
		if !cp.Existed {
			verb = "removed "
		}
		done = append(done, verb+cp.Path)
	}
	if len(failed) > 0 {
		return done, fmt.Errorf("not restored: %s", strings.Join(failed, "; "))
	}
	return done, nil
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
