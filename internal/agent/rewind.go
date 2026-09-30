package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/tools"
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
	// Mode is the file's permission bits, so a restore keeps them.
	Mode uint32 `json:"mode,omitempty"`
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

// RestoreFile is how a rewind puts one file back: path to data with mode
// (0 for owner-only), or removed when existed is false. It is the
// session's, confined to its roots.
type RestoreFile func(path string, data []byte, existed bool, mode os.FileMode) error

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
		// The person's own write, recorded as their action: the request, the
		// policy's decision (an ask is theirs to answer, and they did), and
		// what came of it.
		id := "restore-" + newID()
		args, _ := json.Marshal(map[string]string{"path": cp.Path, "restore_from": "checkpoint " + strconv.FormatInt(cp.Seq, 10)})
		decision := l.Policy.Evaluate("write", true, args)
		refused, err := l.manualDecide("write", id, args, decision, Unanswered)
		if err != nil {
			return done, err
		}
		if refused != nil {
			_ = l.ManualObserve(id, "write", *refused, 0)
			failed = append(failed, fmt.Sprintf("%s: refused: %s", cp.Path, decision.Reason))
			continue
		}
		start := time.Now()
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
		if err := restore(cp.Path, data, cp.Existed, cp.Mode); err != nil {
			_ = l.ManualObserve(id, "write", tools.Result{Content: err.Error(), IsError: true}, time.Since(start))
			failed = append(failed, fmt.Sprintf("%s: %v", cp.Path, err))
			continue
		}
		after := ""
		if cp.Existed {
			after = hashOf(data)
		}
		verb := "restored "
		if !cp.Existed {
			verb = "removed "
		}
		if err := l.ManualObserve(id, "write", tools.Result{Content: verb + cp.Path}, time.Since(start)); err != nil {
			return done, err
		}
		if _, err := l.Recorder.Record(EvFileRestored, ActorUser, Trusted, FileRestored{
			Path: cp.Path, BeforeSHA256: before, AfterSHA256: after,
			Checkpoint: strconv.FormatInt(cp.Seq, 10), By: "user",
		}); err != nil {
			return done, err
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

// BranchCopy is what a branch of a session copies, as the new session's
// events from seq first on: the conversation as it stands through step
// through (0 for all of it), forks applied, and every checkpoint and
// restore up to that step, abandoned branches included, since the files
// they changed are still as they left them. Streamed fragments stay behind.
// Each copy gets a new id and seq, and a restore's checkpoint is renumbered
// to match, so the branch's undo log is the source's.
func BranchCopy(events []Event, through int64, sessionID string, first int64) []Event {
	keep := map[int64]bool{}
	for _, ev := range Live(events) {
		keep[ev.Seq] = true
	}
	var picked []Event
	for _, ev := range events {
		if through > 0 && ev.Seq > through {
			continue
		}
		switch {
		case ev.Type == EvAgentDelta || ev.Type == EvAgentReasoningDelta || ev.Type == EvForked:
			continue
		case keep[ev.Seq], ev.Type == EvCheckpoint, ev.Type == EvFileRestored:
			picked = append(picked, ev)
		}
	}
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].Seq < picked[j].Seq })
	old := make([]int64, len(picked))
	for i := range picked {
		old[i] = picked[i].Seq
	}
	// renumber maps a source seq to the first copied seq at or after it.
	renumber := func(seq int64) int64 {
		i := sort.Search(len(old), func(i int) bool { return old[i] >= seq })
		return first + int64(i)
	}
	out := make([]Event, len(picked))
	for i, ev := range picked {
		ev.ID, ev.SessionID, ev.ParentID, ev.Seq = newID(), sessionID, "", first+int64(i)
		if ev.Type == EvFileRestored {
			var r FileRestored
			if json.Unmarshal(ev.Payload, &r) == nil {
				if used, err := strconv.ParseInt(r.Checkpoint, 10, 64); err == nil {
					r.Checkpoint = strconv.FormatInt(renumber(used), 10)
					if raw, err := json.Marshal(r); err == nil {
						ev.Payload = raw
					}
				}
			}
		}
		out[i] = ev
	}
	return out
}
