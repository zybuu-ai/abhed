package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

func openTest(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(Options{Dir: dir, User: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// record writes n user messages to a new session through a Recorder, as the
// loop does, and returns the session id.
func record(t *testing.T, s *Store, id string, texts ...string) *agent.Recorder {
	t.Helper()
	if err := s.CreateSession(context.Background(), store.SessionRecord{ID: id, User: "tester", Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	rec := agent.NewRecorder(s, id, "")
	for _, text := range texts {
		if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	return rec
}

func mustVerify(t *testing.T, s *Store, id string) Report {
	t.Helper()
	rep, err := s.Verify(id)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("verify %s failed: %+v", id, rep)
	}
	return rep
}

func TestAppendReadBackAndVerify(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one", "two <&> three", "four")

	evs, err := s.Events("s-1")
	if err != nil || len(evs) != 3 {
		t.Fatalf("events = %d, %v", len(evs), err)
	}
	var m agent.Message
	_ = json.Unmarshal(evs[1].Payload, &m)
	if m.Text != "two <&> three" {
		t.Fatalf("payload round trip: %q", m.Text)
	}
	rep := mustVerify(t, s, "s-1")
	if rep.Events != 3 || rep.Head.Seq != 3 || rep.Head.Lines != 3 {
		t.Fatalf("report: %+v", rep)
	}
	// Each line names the one before it.
	data, _ := os.ReadFile(s.Path("s-1"))
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	var first, second line
	_ = json.Unmarshal(lines[0], &first)
	_ = json.Unmarshal(lines[1], &second)
	if first.Prev != Genesis || second.Prev != first.Hash || !isHash(second.Hash) {
		t.Fatalf("chain links: %s → %s → %s", first.Prev, first.Hash, second.Prev)
	}
	// Read by another process's store, from the file.
	other := openTest(t, dir)
	evs2, _ := other.Events("s-1")
	if len(evs2) != 3 || evs2[2].ID != evs[2].ID {
		t.Fatalf("read from file: %+v", evs2)
	}
}

func TestPermissionsAreOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps the record private with the profile's access list")
	}
	old := setUmask(0)
	defer setUmask(old)
	dir := filepath.Join(t.TempDir(), "records")
	s := openTest(t, dir)
	record(t, s, "s-1", "hello")
	if _, err := s.Blobs().Put([]byte("pre-image")); err != nil {
		t.Fatal(err)
	}
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s is %o, want %o", p, got, want)
		}
		return nil
	})
	// A wider directory is made private again on open.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	openTest(t, dir)
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Fatalf("records dir left at %o", info.Mode().Perm())
	}
}

func TestReplayAndStepTaken(t *testing.T) {
	s := openTest(t, t.TempDir())
	rec := record(t, s, "s-1", "one")
	evs, _ := s.Events("s-1")
	if err := s.Append(evs[0]); err != nil {
		t.Fatalf("a replay of the same event is success: %v", err)
	}
	other := evs[0]
	other.ID = agent.NewEventID()
	if err := s.Append(other); !errors.Is(err, agent.ErrStepTaken) {
		t.Fatalf("another event at a held seq: %v", err)
	}
	_ = rec
	mustVerify(t, s, "s-1")
}

func TestOutOfOrderSeqsKeepTheChain(t *testing.T) {
	s := openTest(t, t.TempDir())
	record(t, s, "s-1")
	for _, seq := range []int64{2, 1, 4, 3} {
		ev := agent.Event{ID: agent.NewEventID(), SessionID: "s-1", Seq: seq, Type: agent.EvUserMessage,
			Payload: json.RawMessage(fmt.Sprintf(`{"text":"%d"}`, seq)), Actor: agent.ActorUser, Trust: agent.Trusted, CreatedAt: time.Now()}
		if err := s.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := s.Events("s-1")
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("events not in seq order: %d at %d", e.Seq, i)
		}
	}
	mustVerify(t, s, "s-1")
}

// tamper rewrites a session's file with change applied to its lines.
func tamper(t *testing.T, s *Store, id string, change func(lines [][]byte) [][]byte) {
	t.Helper()
	p := s.Path(id)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	lines = change(lines)
	out := bytes.Join(lines, []byte("\n"))
	if len(lines) > 0 {
		out = append(out, '\n')
	}
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyNamesTheTamperedEvent(t *testing.T) {
	cases := []struct {
		name     string
		change   func([][]byte) [][]byte
		firstBad int64
		reason   string
	}{
		{"edit a payload", func(l [][]byte) [][]byte {
			l[2] = bytes.Replace(l[2], []byte("three"), []byte("THREE"), 1)
			return l
		}, 3, "hash"},
		{"edit and re-hash one line", func(l [][]byte) [][]byte {
			var x line
			_ = json.Unmarshal(l[1], &x)
			x.Payload = json.RawMessage(`{"text":"forged"}`)
			raw, _ := x.seal()
			l[1] = raw
			return l
		}, 3, "follow"},
		{"delete a line", func(l [][]byte) [][]byte { return append(l[:1], l[2:]...) }, 3, "follow"},
		{"reorder two lines", func(l [][]byte) [][]byte {
			l[1], l[2] = l[2], l[1]
			return l
		}, 3, "follow"},
		{"add whitespace", func(l [][]byte) [][]byte {
			l[3] = bytes.Replace(l[3], []byte(`,"type"`), []byte(`, "type"`), 1)
			return l
		}, 4, "written"},
		{"truncate the end", func(l [][]byte) [][]byte { return l[:3] }, 4, "missing"},
		{"delete the first line", func(l [][]byte) [][]byte { return l[1:] }, 2, "follow"},
		{"empty the file", func(l [][]byte) [][]byte { return nil }, 1, "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t, t.TempDir())
			record(t, s, "s-1", "one", "two", "three", "four", "five")
			evs, _ := s.Events("s-1")
			if err := s.Release("s-1"); err != nil {
				t.Fatal(err)
			}
			tamper(t, s, "s-1", tc.change)
			rep, err := s.Verify("s-1")
			if err != nil {
				t.Fatal(err)
			}
			if rep.OK || rep.FirstBad != tc.firstBad || !strings.Contains(rep.Reason, tc.reason) {
				t.Fatalf("report %+v, want first bad %d (%s)", rep, tc.firstBad, tc.reason)
			}
			if rep.Line > 0 && rep.EventID != "" && rep.EventID != evs[rep.FirstBad-1].ID && tc.name != "reorder two lines" && tc.name != "delete a line" && tc.name != "delete the first line" {
				t.Fatalf("named event %s, want %s", rep.EventID, evs[rep.FirstBad-1].ID)
			}
		})
	}
}

// A record cut back and given a matching head file still disagrees with
// the heads the index kept at each end.
func TestVerifyCatchesTruncationBehindANewHead(t *testing.T) {
	s := openTest(t, t.TempDir())
	rec := record(t, s, "s-1", "one", "two")
	if _, err := rec.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted}); err != nil {
		t.Fatal(err)
	}
	_ = s.Release("s-1")
	tamper(t, s, "s-1", func(l [][]byte) [][]byte { return l[:2] })
	var x line
	data, _ := os.ReadFile(s.Path("s-1"))
	_ = json.Unmarshal(bytes.Split(data, []byte("\n"))[1], &x)
	if err := s.writeHead("s-1", Head{Lines: 2, Seq: x.Seq, Hash: x.Hash}); err != nil {
		t.Fatal(err)
	}
	rep, _ := s.Verify("s-1")
	if rep.OK || !strings.Contains(rep.Reason, "index") {
		t.Fatalf("report %+v", rep)
	}
}

func TestVerifyAllFindsAMissingSessionFile(t *testing.T) {
	s := openTest(t, t.TempDir())
	record(t, s, "s-1", "one")
	record(t, s, "s-2", "two")
	_ = s.Release("s-1")
	if err := os.Remove(s.Path("s-1")); err != nil {
		t.Fatal(err)
	}
	idx, reps, err := s.VerifyAll()
	if err != nil || !idx.OK {
		t.Fatalf("index %+v %v", idx, err)
	}
	bad := 0
	for _, r := range reps {
		if !r.OK {
			bad++
			if r.ID != "s-1" || !strings.Contains(r.Reason, "missing") {
				t.Fatalf("report %+v", r)
			}
		}
	}
	if bad != 1 {
		t.Fatalf("%d bad reports", bad)
	}
}

func TestVerifyIndexTamper(t *testing.T) {
	for name, change := range map[string]func([][]byte) [][]byte{
		"edit": func(l [][]byte) [][]byte {
			l[0] = bytes.Replace(l[0], []byte("tester"), []byte("someone"), 1)
			return l
		},
		"delete":   func(l [][]byte) [][]byte { return append(l[:1], l[2:]...) },
		"truncate": func(l [][]byte) [][]byte { return l[:len(l)-1] },
		"reorder":  func(l [][]byte) [][]byte { l[0], l[1] = l[1], l[0]; return l },
	} {
		t.Run(name, func(t *testing.T) {
			s := openTest(t, t.TempDir())
			record(t, s, "s-1", "one")
			record(t, s, "s-2", "two")
			if rep, _ := s.VerifyIndex(); !rep.OK {
				t.Fatalf("clean index: %+v", rep)
			}
			p := s.index.path()
			data, _ := os.ReadFile(p)
			lines := change(bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")))
			_ = os.WriteFile(p, append(bytes.Join(lines, []byte("\n")), '\n'), 0o600)
			if rep, _ := s.VerifyIndex(); rep.OK {
				t.Fatalf("tampered index verified: %+v", rep)
			}
		})
	}
}

func TestTornTailIsRepairedAndRecorded(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one", "two")
	_ = s.Release("s-1")
	// A crash mid-write: part of a line and no newline.
	f, _ := os.OpenFile(s.Path("s-1"), os.O_APPEND|os.O_WRONLY, 0o600)
	torn := `{"seq":3,"id":"01J","session_id":"s-1","ty`
	_, _ = f.WriteString(torn)
	_ = f.Close()

	rep, _ := s.Verify("s-1")
	if !rep.OK || rep.Torn == 0 {
		t.Fatalf("a torn tail is noted, not a failure: %+v", rep)
	}
	// A read leaves it; the next writer cuts it.
	if evs, _ := s.Events("s-1"); len(evs) != 2 {
		t.Fatalf("read: %d events", len(evs))
	}
	if data, _ := os.ReadFile(s.Path("s-1")); !bytes.HasSuffix(data, []byte(torn)) {
		t.Fatal("a read changed the record")
	}
	if err := s.Acquire("s-1"); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Events("s-1")
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.Type != agent.EvRecordRepaired || last.Seq != 3 || last.Actor != agent.ActorSystem {
		t.Fatalf("last event %+v", last)
	}
	var rr agent.RecordRepaired
	_ = json.Unmarshal(last.Payload, &rr)
	if rr.TruncatedBytes != int64(len(torn)) || rr.Reason == "" {
		t.Fatalf("repair payload %+v", rr)
	}
	rep = mustVerify(t, s, "s-1")
	if rep.Torn != 0 {
		t.Fatalf("still torn: %+v", rep)
	}
	// A recorder continuing from the record goes on after the repair.
	rec := agent.NewRecorder(s, "s-1", "")
	rec.Advance(evs[len(evs)-1].Seq)
	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "after"}); err != nil {
		t.Fatal(err)
	}
	mustVerify(t, s, "s-1")
}

func TestTwoWritersOneRefused(t *testing.T) {
	dir := t.TempDir()
	a := openTest(t, dir)
	b := openTest(t, dir)
	record(t, a, "s-1", "one")

	ev := agent.Event{ID: agent.NewEventID(), SessionID: "s-1", Seq: 2, Type: agent.EvUserMessage,
		Payload: json.RawMessage(`{"text":"b"}`), Actor: agent.ActorUser, Trust: agent.Trusted, CreatedAt: time.Now()}
	if err := b.Append(ev); !errors.Is(err, ErrHeldElsewhere) {
		t.Fatalf("second writer: %v", err)
	}
	if ok, err := b.ClaimResume(context.Background(), "s-1"); ok || err != nil {
		t.Fatalf("second claim: %v %v", ok, err)
	}
	if rec, _ := b.GetSession(context.Background(), "s-1"); rec.EndedAt != nil {
		t.Fatalf("held elsewhere reads as running: %+v", rec)
	}
	// The writer lets it go; the other takes it and goes on.
	if err := a.Release("s-1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := b.ClaimResume(context.Background(), "s-1"); !ok || err != nil {
		t.Fatalf("claim after release: %v %v", ok, err)
	}
	if err := b.Append(ev); err != nil {
		t.Fatal(err)
	}
	if err := a.Append(ev); !errors.Is(err, ErrHeldElsewhere) {
		t.Fatalf("first writer after the handover: %v", err)
	}
	mustVerify(t, b, "s-1")
}

func TestClaimWhileRunningInThisProcess(t *testing.T) {
	s := openTest(t, t.TempDir())
	rec := record(t, s, "s-1", "one")
	if ok, _ := s.ClaimResume(context.Background(), "s-1"); ok {
		t.Fatal("a session still running here was claimed again")
	}
	if _, err := rec.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ClaimResume(context.Background(), "s-1"); !ok {
		t.Fatal("an ended session was not claimed")
	}
	if ok, _ := s.ClaimResume(context.Background(), "unknown"); ok {
		t.Fatal("an unknown session was claimed")
	}
}

type canary struct{}

func (canary) Redact(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("sk-CANARY-4242"), []byte("[secret:API_KEY]"))
}
func (canary) Span() int { return len("sk-CANARY-4242") }

func TestRedactedBeforeTheFirstWrite(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Options{Dir: dir, User: "tester", Redact: canary{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateSession(context.Background(), store.SessionRecord{ID: "s-1", Workspace: dir, Prompt: "use sk-CANARY-4242 please"}); err != nil {
		t.Fatal(err)
	}
	// A recorder with no redactor of its own: the store's still applies.
	rec := agent.NewRecorder(s, "s-1", "")
	_, _ = rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "key sk-CANARY-4242"})
	_, _ = rec.Record(agent.EvObservation, agent.ActorTool, agent.Untrusted, agent.Observation{Content: "echo sk-CANARY-4242"})
	var buf bytes.Buffer
	if _, err := s.Export("s-1", &buf, ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if strings.Contains(buf.String(), "sk-CANARY") {
		t.Fatal("the canary is in the export")
	}
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			data, _ := os.ReadFile(p)
			if bytes.Contains(data, []byte("sk-CANARY")) {
				t.Errorf("the canary is on disk in %s", p)
			}
		}
		return nil
	})
	s2 := openTest(t, dir)
	mustVerify(t, s2, "s-1")
}

func TestBlobs(t *testing.T) {
	s := openTest(t, t.TempDir())
	sha, err := s.Blobs().Put([]byte("before the edit"))
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Blobs().Put([]byte("before the edit")); again != sha {
		t.Fatal("content address changed")
	}
	got, err := s.Blobs().Get(sha)
	if err != nil || string(got) != "before the edit" {
		t.Fatalf("get: %q %v", got, err)
	}
	if err := os.WriteFile(s.blobs.path(sha), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Blobs().Get(sha); !errors.Is(err, ErrBlobDamaged) {
		t.Fatalf("a changed blob was returned: %v", err)
	}
	if _, err := s.Blobs().Get("../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a path was taken as a hash: %v", err)
	}
}

func TestIndexListResolveAndName(t *testing.T) {
	s := openTest(t, t.TempDir())
	ws := t.TempDir()
	other := t.TempDir()
	for i, cwd := range []string{ws, ws, other} {
		id := fmt.Sprintf("s-%d", i+1)
		if err := s.CreateSession(context.Background(), store.SessionRecord{ID: id, Workspace: cwd, User: "tester"}); err != nil {
			t.Fatal(err)
		}
		rec := agent.NewRecorder(s, id, "")
		_, _ = rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: fmt.Sprintf("task %d\nmore", i+1)})
		time.Sleep(2 * time.Millisecond)
	}
	rec := agent.NewRecorder(s, "s-1", "")
	rec.Advance(1)
	_, _ = rec.Record(agent.EvSessionNamed, agent.ActorUser, agent.Trusted, agent.SessionNamed{Name: "auth-fix"})

	mine, _ := s.Index().List(Filter{Cwd: ws})
	if len(mine) != 2 || mine[0].ID != "s-1" || mine[0].Title != "task 1" || mine[0].Name != "auth-fix" {
		t.Fatalf("list: %+v", mine)
	}
	all, _ := s.Index().List(Filter{All: true})
	if len(all) != 3 {
		t.Fatalf("all: %d", len(all))
	}
	for key, want := range map[string]string{"auth-fix": "s-1", "s-2": "s-2", s.Path("s-3"): "s-3"} {
		e, err := s.Index().Resolve(key)
		if err != nil || e.ID != want {
			t.Fatalf("resolve %q: %+v %v", key, e, err)
		}
	}
	if _, err := s.Index().Resolve("s-"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a short prefix: %v", err)
	}
	_ = s.CreateSession(context.Background(), store.SessionRecord{ID: "abcd1", Workspace: ws})
	_ = s.CreateSession(context.Background(), store.SessionRecord{ID: "abcd2", Workspace: ws})
	if _, err := s.Index().Resolve("abcd"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("an ambiguous prefix: %v", err)
	}
	if seq, hash, err := s.Index().Head("s-1"); err != nil || seq != 2 || !isHash(hash) {
		t.Fatalf("head: %d %s %v", seq, hash, err)
	}
	// A subagent's own session is never listed.
	_ = s.CreateSubagentSession(context.Background(), "child1", "s-1", "look around")
	if mine, _ := s.Index().List(Filter{Cwd: ws}); len(mine) != 4 {
		t.Fatalf("list with a subagent: %d", len(mine))
	}
	if ok, _ := s.SubSessionOf(context.Background(), "child1", "s-1"); !ok {
		t.Fatal("child not its parent's")
	}
	if ok, _ := s.SubSessionOf(context.Background(), "child1", "s-2"); ok {
		t.Fatal("child claimed by another session")
	}
}

func TestRepoWidensToWorktrees(t *testing.T) {
	main := t.TempDir()
	if err := os.MkdirAll(filepath.Join(main, ".git", "worktrees", "wt"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(main, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600)
	wt := t.TempDir()
	wtGit := filepath.Join(main, ".git", "worktrees", "wt")
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o600)
	_ = os.WriteFile(filepath.Join(wtGit, "HEAD"), []byte("ref: refs/heads/feature\n"), 0o600)
	if RepoOf(main) == "" || RepoOf(main) != RepoOf(wt) {
		t.Fatalf("repo: %q vs %q", RepoOf(main), RepoOf(wt))
	}
	if BranchOf(wt) != "feature" || BranchOf(main) != "main" {
		t.Fatalf("branches: %q %q", BranchOf(wt), BranchOf(main))
	}
	s := openTest(t, t.TempDir())
	_ = s.CreateSession(context.Background(), store.SessionRecord{ID: "s-main", Workspace: main})
	_ = s.CreateSession(context.Background(), store.SessionRecord{ID: "s-wt", Workspace: wt})
	if l, _ := s.Index().List(Filter{Cwd: main}); len(l) != 1 {
		t.Fatalf("this workspace: %d", len(l))
	}
	if l, _ := s.Index().List(Filter{Cwd: main, Repo: true}); len(l) != 2 {
		t.Fatalf("the repository: %d", len(l))
	}
}

func TestPruneWritesATombstone(t *testing.T) {
	s := openTest(t, t.TempDir())
	rec := record(t, s, "s-1", "one")
	sha, _ := s.Blobs().Put([]byte("only s-1 names this"))
	shared, _ := s.Blobs().Put([]byte("both name this"))
	_, _ = rec.Record(agent.EvCheckpoint, agent.ActorSystem, agent.Trusted, agent.CheckpointSaved{Path: "a", SHA256: sha})
	_, _ = rec.Record(agent.EvCheckpoint, agent.ActorSystem, agent.Trusted, agent.CheckpointSaved{Path: "b", SHA256: shared})
	rec2 := record(t, s, "s-2", "two")
	_, _ = rec2.Record(agent.EvCheckpoint, agent.ActorSystem, agent.Trusted, agent.CheckpointSaved{Path: "b", SHA256: shared})
	_ = s.CreateSubagentSession(context.Background(), "child1", "s-1", "sub")
	head := mustVerify(t, s, "s-1").Head

	if _, err := s.Prune("s-1", "user", "by hand"); err == nil {
		t.Fatal("a session open in this process was pruned")
	}
	_ = s.Release("s-1")
	_ = s.Release("child1")
	pruned, err := s.Prune("s-1", "user", "by hand")
	if err != nil || len(pruned) != 2 || pruned[0].Head != head || pruned[0].Blobs != 1 {
		t.Fatalf("prune: %+v %v", pruned, err)
	}
	for _, p := range []string{s.Path("s-1"), s.headPath("s-1"), s.Path("child1")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind", p)
		}
	}
	if _, err := s.Blobs().Get(sha); !errors.Is(err, ErrNotFound) {
		t.Fatal("an unshared checkpoint was kept")
	}
	if _, err := s.Blobs().Get(shared); err != nil {
		t.Fatal("a checkpoint another session names was removed")
	}
	e, _ := s.index.get("s-1")
	if !e.Pruned || e.Head != head {
		t.Fatalf("tombstone: %+v", e)
	}
	if l, _ := s.Index().List(Filter{All: true}); len(l) != 1 {
		t.Fatalf("pruned session still listed: %+v", l)
	}
	ev := agent.Event{ID: agent.NewEventID(), SessionID: "s-1", Seq: 9, Type: agent.EvUserMessage, Payload: json.RawMessage(`{}`), Actor: agent.ActorUser, Trust: agent.Trusted}
	if err := s.Append(ev); err == nil {
		t.Fatal("a pruned session was written again")
	}
	idx, reps, err := s.VerifyAll()
	if err != nil || !idx.OK {
		t.Fatalf("index after prune: %+v %v", idx, err)
	}
	for _, r := range reps {
		if !r.OK {
			t.Fatalf("after prune: %+v", r)
		}
	}
}

func TestExportVerifiesOffline(t *testing.T) {
	s := openTest(t, t.TempDir())
	record(t, s, "s-1", "one", "two", "three")
	p := filepath.Join(t.TempDir(), "s-1.jsonl")
	var buf bytes.Buffer
	if _, err := s.Export("s-1", &buf, ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(p, buf.Bytes(), 0o600)
	if rep, err := VerifyFile(p); err != nil || !rep.OK || rep.Events != 3 {
		t.Fatalf("export: %+v %v", rep, err)
	}
	lines := bytes.Split(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), []byte("\n"))
	// Cut the last event but keep the trailer: the head no longer matches.
	cut := append(append([][]byte{}, lines[:2]...), lines[3])
	_ = os.WriteFile(p, append(bytes.Join(cut, []byte("\n")), '\n'), 0o600)
	if rep, _ := VerifyFile(p); rep.OK {
		t.Fatalf("a cut export verified: %+v", rep)
	}
	// Without the trailer, what is there verifies, with a note.
	_ = os.WriteFile(p, append(bytes.Join(lines[:3], []byte("\n")), '\n'), 0o600)
	if rep, _ := VerifyFile(p); !rep.OK || len(rep.Notes) == 0 {
		t.Fatalf("no trailer: %+v", rep)
	}
}

func TestIDsMustBePlainNames(t *testing.T) {
	s := openTest(t, t.TempDir())
	for _, id := range []string{"../x", "a/b", "", ".hidden", strings.Repeat("a", 200)} {
		if err := s.CreateSession(context.Background(), store.SessionRecord{ID: id}); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
	if _, err := Open(Options{Dir: t.TempDir(), Tenant: "../up"}); err == nil {
		t.Error("tenant ../up accepted")
	}
}

func TestPruneOlderKeepsRecentAndHeldSessions(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	old := time.Now().Add(-100 * 24 * time.Hour)
	s.index.clock = func() time.Time { return old }
	record(t, s, "s-old", "long ago")
	record(t, s, "s-old-held", "long ago too")
	s.index.clock = nil
	record(t, s, "s-new", "today")
	_ = s.Release("s-old")
	_ = s.Release("s-new")
	// s-old-held stays open in another store, as another process would hold it.
	other := openTest(t, dir)
	_ = s.Release("s-old-held")
	if err := other.Acquire("s-old-held"); err != nil {
		t.Fatal(err)
	}
	pruned, skipped, err := s.PruneOlder(time.Now().Add(-90*24*time.Hour), "managed", "record.retention_days is 90")
	if err != nil || len(pruned) != 1 || pruned[0].ID != "s-old" || len(skipped) != 1 || skipped[0] != "s-old-held" {
		t.Fatalf("pruned %+v skipped %v err %v", pruned, skipped, err)
	}
}

// The local record offers no way to delete or rewrite a session: it is not
// a SessionDeleter, and prune, the one way out, leaves a tombstone.
func TestNoDeletePath(t *testing.T) {
	var s any = &Store{}
	if _, ok := s.(agent.SessionDeleter); ok {
		t.Fatal("the local record can delete a session")
	}
}

// The anchor hook sees every head a sync moves, a session's and the index's.
func TestAnchorSeesEachHead(t *testing.T) {
	var got []string
	s, err := Open(Options{Dir: t.TempDir(), Anchor: func(tenant, session string, h Head) {
		got = append(got, fmt.Sprintf("%s/%s/%d", tenant, session, h.Lines))
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record(t, s, "s-1", "one", "two")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "default/s-1/1") || !strings.Contains(joined, "default/s-1/2") || !strings.Contains(joined, "default/index/") {
		t.Fatalf("anchored: %v", got)
	}
}
