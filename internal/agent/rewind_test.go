package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Rewinding to before the first prompt is a recorded fork at 0 in the same
// session: the loop stays, the conversation is empty, and a rebuild from the
// record agrees.
func TestForkBeforeTheFirstPromptIsAForkAtZero(t *testing.T) {
	store := NewMemStore()
	sess, _ := tools.NewSession(tempDir(t))
	l := NewLoop(nil, nil, nil, nil, sess, NewRecorder(store, "s", ""), DefaultConfig())
	for _, e := range []Event{
		ev(1, EvSessionStarted, map[string]string{}),
		ev(2, EvUserMessage, Message{Text: "one"}),
		ev(3, EvAgentMessage, Message{Text: "ok"}),
	} {
		e.SessionID = "s"
		_ = store.Append(e)
	}
	l.Recorder.Advance(3)
	events, _ := store.Events("s")
	if n, err := l.ForkBefore(events, 2); err != nil || n != 0 {
		t.Fatalf("ForkBefore = %d, %v", n, err)
	}
	events, _ = store.Events("s")
	last := events[len(events)-1]
	var f Forked
	if last.Type != EvForked || json.Unmarshal(last.Payload, &f) != nil || f.ThroughSeq != 0 || last.Actor != ActorUser {
		t.Fatalf("recorded %s %s", last.Type, last.Payload)
	}
	if len(Live(events)) != 0 {
		t.Fatalf("the abandoned steps are still live: %v", Live(events))
	}
	if len(events) != 4 {
		t.Fatal("a rewind deleted events")
	}
	if len(l.messages) != 0 {
		t.Fatal("the conversation was kept")
	}
}

// ForkBefore a later prompt forks through the last step before it.
func TestForkBeforeALaterPrompt(t *testing.T) {
	store := NewMemStore()
	sess, _ := tools.NewSession(tempDir(t))
	l := NewLoop(nil, nil, nil, nil, sess, NewRecorder(store, "s", ""), DefaultConfig())
	for _, e := range []Event{
		ev(1, EvUserMessage, Message{Text: "one"}),
		ev(2, EvAgentMessage, Message{Text: "ok one"}),
		ev(3, EvSessionEnded, SessionEnded{Reason: TermCompleted}),
		ev(4, EvUserMessage, Message{Text: "two"}),
		ev(5, EvAgentMessage, Message{Text: "ok two"}),
	} {
		e.SessionID = "s"
		_ = store.Append(e)
	}
	l.Recorder.Advance(5)
	events, _ := store.Events("s")
	if n, err := l.ForkBefore(events, 4); err != nil || n != 2 {
		t.Fatalf("ForkBefore = %d, %v", n, err)
	}
	events, _ = store.Events("s")
	msgs, _ := Fork(events, 0)
	if got := dump(msgs); strings.Contains(got, "two") || !strings.Contains(got, "ok one") {
		t.Fatalf("rebuilt: %s", got)
	}
	pts := RewindPoints(events)
	if len(pts) != 1 || pts[0].Text != "one" {
		t.Fatalf("points: %+v", pts)
	}
}

// The undo log rebuilt from a record holds the checkpoints no restore used,
// reads their content from the blobs, and Since takes the earliest per file.
func TestUndoLogRebuildAndSince(t *testing.T) {
	blobs := map[string][]byte{"h1": []byte("a0"), "h2": []byte("a1"), "h3": []byte("b0")}
	get := func(sha string) ([]byte, error) {
		if b, ok := blobs[sha]; ok {
			return b, nil
		}
		return nil, errors.New("no blob")
	}
	events := []Event{
		ev(1, EvUserMessage, Message{Text: "one"}),
		ev(2, EvCheckpoint, CheckpointSaved{Path: "/w/a", SHA256: "h1", Turn: 1}),
		ev(3, EvUserMessage, Message{Text: "two"}),
		ev(4, EvCheckpoint, CheckpointSaved{Path: "/w/a", SHA256: "h2", Turn: 2}),
		ev(5, EvCheckpoint, CheckpointSaved{Path: "/w/new", Turn: 2}),
		ev(6, EvUserMessage, Message{Text: "three"}),
		ev(7, EvCheckpoint, CheckpointSaved{Path: "/w/b", SHA256: "h3", Turn: 3}),
		ev(8, EvFileRestored, FileRestored{Path: "/w/b", Checkpoint: "7", By: "user"}),
	}
	u := NewUndoLog(nil, nil)
	u.Rebuild(events, get)
	if u.Pending() != 2 {
		t.Fatalf("pending turns %d, want 2 (turn 3 was restored)", u.Pending())
	}
	if since, ok := u.LastTurnStart(); !ok || since != 3 {
		t.Fatalf("last turn starts after %d, %v", since, ok)
	}
	if u.Peek(3) != 2 || u.Peek(0) != 3 {
		t.Fatalf("peek: %d %d", u.Peek(3), u.Peek(0))
	}
	cps := u.Since(1)
	if len(cps) != 2 || cps[0].Path != "/w/a" || cps[0].Seq != 2 || cps[1].Path != "/w/new" || cps[1].Existed {
		t.Fatalf("since: %+v", cps)
	}
	if data, err := cps[0].content(); err != nil || string(data) != "a0" {
		t.Fatalf("content from the record: %q %v", data, err)
	}
	if u.Pending() != 0 {
		t.Fatal("Since left what it took")
	}
}

// Restores are the person's, checked by policy: a denied path is recorded
// as refused and left alone; the rest are file.restored with both hashes.
func TestRestoreCheckpointsRecordsAndRespectsDeny(t *testing.T) {
	dir := tempDir(t)
	sess, _ := tools.NewSession(dir)
	store := NewMemStore()
	pol := policy.New(policy.ModeDefault)
	if err := pol.AddDeny("write(" + filepath.Join(dir, "keep.txt") + ")"); err != nil {
		t.Fatal(err)
	}
	l := NewLoop(nil, nil, pol, nil, sess, NewRecorder(store, "s", ""), DefaultConfig())
	a, keep := filepath.Join(dir, "a.txt"), filepath.Join(dir, "keep.txt")
	_ = os.WriteFile(a, []byte("changed"), 0o600)
	_ = os.WriteFile(keep, []byte("changed"), 0o600)
	cps := []Checkpoint{
		{Path: a, Before: []byte("original"), Existed: true, Seq: 5},
		{Path: keep, Before: []byte("original"), Existed: true, Seq: 6},
	}
	current := func(p string) ([]byte, bool) { b, err := os.ReadFile(p); return b, err == nil }
	restore := func(p string, data []byte, existed bool, _ os.FileMode) error { return sess.RestoreFile(p, data) }
	done, err := l.RestoreCheckpoints(cps, current, restore, nil)
	if err == nil || len(done) != 1 {
		t.Fatalf("done %v err %v", done, err)
	}
	if b, _ := os.ReadFile(a); string(b) != "original" {
		t.Fatalf("a.txt = %q", b)
	}
	if b, _ := os.ReadFile(keep); string(b) != "changed" {
		t.Fatal("a denied path was restored")
	}
	events, _ := store.Events("s")
	var restored, denied int
	for _, e := range events {
		switch e.Type {
		case EvFileRestored:
			restored++
			var r FileRestored
			_ = json.Unmarshal(e.Payload, &r)
			if r.BeforeSHA256 != hashOf([]byte("changed")) || r.AfterSHA256 != hashOf([]byte("original")) || r.Checkpoint != "5" || r.By != "user" || e.Actor != ActorUser {
				t.Fatalf("file.restored %+v", r)
			}
		case EvActionDenied:
			denied++
		}
	}
	if restored != 1 || denied != 1 {
		t.Fatalf("%d restored, %d denied", restored, denied)
	}
}

func TestBlobRefs(t *testing.T) {
	refs := BlobRefs([]Event{
		ev(1, EvCheckpoint, CheckpointSaved{Path: "a", SHA256: "x"}),
		ev(2, EvCheckpoint, CheckpointSaved{Path: "b"}),
		ev(3, EvFileRestored, FileRestored{Path: "a", BeforeSHA256: "y", AfterSHA256: "x"}),
	})
	if strings.Join(refs, ",") != "x,y,x" {
		t.Fatalf("refs %v", refs)
	}
}

// A checkpoint's path comes from the record; one that names a file outside
// the session's roots is not written, whatever the record says.
func TestRestoreStaysInTheSession(t *testing.T) {
	dir, outside := tempDir(t), tempDir(t)
	sess, _ := tools.NewSession(dir)
	l := NewLoop(nil, nil, policy.New(policy.ModeDefault), nil, sess, NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	target := filepath.Join(outside, "x.txt")
	_ = os.WriteFile(target, []byte("theirs"), 0o600)
	cps := []Checkpoint{{Path: target, Before: []byte("planted"), Existed: true, Seq: 3}}
	current := func(p string) ([]byte, bool) { b, err := sess.ReadFile(p); return b, err == nil }
	restore := func(p string, data []byte, existed bool, _ os.FileMode) error { return sess.RestoreFile(p, data) }
	if _, err := l.RestoreCheckpoints(cps, current, restore, nil); err == nil {
		t.Fatal("a restore outside the session was taken")
	}
	if b, _ := os.ReadFile(target); string(b) != "theirs" {
		t.Fatalf("the file outside the session was written: %q", b)
	}
}

// The review's probe: a branch renumbers events, and a restore's checkpoint
// must follow, or the branch's undo log offers pre-images already put back.
// The branch rebuilds the source's undo log and conversation exactly.
func TestBranchCopyKeepsTheUndoLog(t *testing.T) {
	get := func(string) ([]byte, error) { return []byte("x"), nil }
	orig := []Event{
		ev(1, EvUserMessage, Message{Text: "one"}),
		ev(2, EvAgentDelta, Delta{Text: "d"}),
		ev(3, EvCheckpoint, CheckpointSaved{Path: "/w/a", SHA256: "h1", Turn: 1}),
		ev(4, EvAgentDelta, Delta{Text: "d"}),
		ev(5, EvCheckpoint, CheckpointSaved{Path: "/w/b", SHA256: "h2", Turn: 1}),
		ev(6, EvFileRestored, FileRestored{Path: "/w/b", Checkpoint: "5", By: "user"}),
		ev(7, EvUserMessage, Message{Text: "two"}),
		ev(8, EvCheckpoint, CheckpointSaved{Path: "/w/c", SHA256: "h3", Turn: 2}),
		ev(9, EvForked, Forked{ThroughSeq: 6}),
		ev(10, EvUserMessage, Message{Text: "three"}),
	}
	u := NewUndoLog(nil, nil)
	u.Rebuild(orig, get)
	br := append([]Event{ev(1, EvSessionBranched, SessionBranched{From: "s", ThroughSeq: 10})}, BranchCopy(orig, 0, "b", 2)...)
	b := NewUndoLog(nil, nil)
	b.Rebuild(br, get)
	if u.Peek(0) != b.Peek(0) || u.Pending() != b.Pending() {
		t.Fatalf("checkpoints pending: original %d, branch %d", u.Peek(0), b.Peek(0))
	}
	ou, bu := u.Since(0), b.Since(0)
	if len(ou) != len(bu) {
		t.Fatalf("since: %+v vs %+v", ou, bu)
	}
	for i := range ou {
		if ou[i].Path != bu[i].Path || ou[i].Blob != bu[i].Blob {
			t.Fatalf("checkpoint %d: %+v vs %+v", i, ou[i], bu[i])
		}
	}
	mo, _ := Fork(orig, 0)
	mb, _ := Fork(br, 0)
	if dump(mo) != dump(mb) {
		t.Fatalf("conversation:\n%s\nvs\n%s", dump(mo), dump(mb))
	}
	for _, e := range br[1:] {
		if e.SessionID != "b" || e.Type == EvAgentDelta || e.Type == EvForked {
			t.Fatalf("copied %+v", e)
		}
	}
	if br[1].ID == orig[0].ID {
		t.Fatal("a copy kept its source id")
	}
}

// A restore is recorded as the person's action, as ManualAs records one: the
// request by the user, the decision, the result; and file.restored. The
// file's mode comes back with its content.
func TestRestoreIsThePersonsRecordedAction(t *testing.T) {
	dir := tempDir(t)
	sess, _ := tools.NewSession(dir)
	store := NewMemStore()
	l := NewLoop(nil, nil, policy.New(policy.ModeDefault), nil, sess, NewRecorder(store, "s", ""), DefaultConfig())
	script := filepath.Join(dir, "run.sh")
	_ = os.WriteFile(script, []byte("changed"), 0o600)
	cps := []Checkpoint{{Path: script, Before: []byte("#!/bin/sh\n"), Existed: true, Seq: 4, Mode: 0o755}}
	current := func(p string) ([]byte, bool) { b, err := sess.ReadFile(p); return b, err == nil }
	restore := func(p string, data []byte, existed bool, mode os.FileMode) error {
		if err := sess.RestoreFile(p, data); err != nil {
			return err
		}
		return os.Chmod(p, mode)
	}
	if _, err := l.RestoreCheckpoints(cps, current, restore, nil); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(script); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	events, _ := store.Events("s")
	var types []string
	for _, e := range events {
		types = append(types, string(e.Type)+"/"+string(e.Actor))
	}
	want := "action.requested/user action.approved/user observation/tool file.restored/user"
	if strings.Join(types, " ") != want {
		t.Fatalf("recorded %v, want %s", types, want)
	}
}
