package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

// cutLines keeps the first keep lines of a file and adds extra, as the
// review's probes cut a record.
func cutLines(t *testing.T, path string, keep int, extra string) {
	t.Helper()
	data, _ := os.ReadFile(path)
	ls := bytes.SplitAfter(data, []byte("\n"))
	out := bytes.Join(ls[:keep], nil)
	out = append(out, extra...)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustFail(t *testing.T, s *Store, why string) {
	t.Helper()
	rep, err := s.Verify("s-1")
	if err != nil || rep.OK {
		t.Fatalf("%s: verify passed: %+v %v", why, rep, err)
	}
}

// Whole lines cut from a session nobody ended: opening it for writing
// refuses, and verify still fails afterwards. Nothing rewrites the head.
func TestTruncateThenOpenIsNotLaundered(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one", "two", "three", "four", "five")
	_ = s.Close()
	s2 := openTest(t, dir)
	cutLines(t, s2.Path("s-1"), 3, "")
	head, _ := os.ReadFile(s2.headPath("s-1"))
	mustFail(t, s2, "cut")
	if err := s2.Acquire("s-1"); !errors.Is(err, ErrUnverified) {
		t.Fatalf("a cut record was opened for writing: %v", err)
	}
	if _, err := s2.ClaimResume(t.Context(), "s-1"); !errors.Is(err, ErrUnverified) {
		t.Fatalf("a cut record was claimed: %v", err)
	}
	if evs, err := s2.Events("s-1"); err != nil || len(evs) != 3 {
		t.Fatalf("it can still be read: %d %v", len(evs), err)
	}
	mustFail(t, s2, "after open")
	if now, _ := os.ReadFile(s2.headPath("s-1")); !bytes.Equal(now, head) {
		t.Fatal("the head was rewritten")
	}
}

// Lines cut with a fragment left behind: a read, as record show does,
// changes nothing, and the fragment is not taken for a crash.
func TestTornCutViaReadPathIsNotLaundered(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one", "two", "three", "four", "five")
	_ = s.Close()
	s2 := openTest(t, dir)
	cutLines(t, s2.Path("s-1"), 2, `{"seq":3,"id":"x`)
	before, _ := os.ReadFile(s2.Path("s-1"))
	if _, err := s2.Events("s-1"); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(s2.Path("s-1")); !bytes.Equal(before, after) {
		t.Fatal("a read changed the record")
	}
	mustFail(t, s2, "after read")
	if err := s2.Acquire("s-1"); !errors.Is(err, ErrUnverified) {
		t.Fatalf("a cut counted line was repaired as a crash: %v", err)
	}
	mustFail(t, s2, "after a refused open")
}

// A counted line that lost only its newline is not torn: it is kept, and a
// line that is whole and chained is completed rather than cut.
func TestMissingNewlineOnACountedLine(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one", "two")
	_ = s.Close()
	s2 := openTest(t, dir)
	data, _ := os.ReadFile(s2.Path("s-1"))
	_ = os.WriteFile(s2.Path("s-1"), bytes.TrimSuffix(data, []byte("\n")), 0o600)
	if err := s2.Acquire("s-1"); err != nil {
		t.Fatalf("a whole last line without its newline: %v", err)
	}
	evs, _ := s2.Events("s-1")
	if len(evs) != 3 || !strings.Contains(string(evs[2].Payload), "newline was completed") {
		t.Fatalf("events: %d %s", len(evs), evs[len(evs)-1].Payload)
	}
	mustVerify(t, s2, "s-1")
}

// The index's last line cut: the next append refuses instead of chaining
// onto the cut and moving the head over it, and verify still fails.
func TestIndexTruncateIsNotLaundered(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	rec := record(t, s, "s-1", "one", "two")
	_, _ = rec.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted})
	_ = s.Close()
	s2 := openTest(t, dir)
	data, _ := os.ReadFile(s2.index.path())
	cutLines(t, s2.index.path(), bytes.Count(data, []byte("\n"))-1, "") // drop the end line
	head, _ := os.ReadFile(s2.index.headPath())
	if err := s2.CreateSession(t.Context(), store.SessionRecord{ID: "s-2"}); !errors.Is(err, ErrIndexDamaged) {
		t.Fatalf("an append onto a cut index: %v", err)
	}
	if rep, _ := s2.VerifyIndex(); rep.OK {
		t.Fatal("LAUNDERED: the cut index verifies")
	}
	if now, _ := os.ReadFile(s2.index.headPath()); !bytes.Equal(now, head) {
		t.Fatal("the index head was rewritten")
	}
}

// A broken chain in the middle of the index, or a missing head, stops
// appends too.
func TestIndexDamageStopsAppends(t *testing.T) {
	for name, damage := range map[string]func(s *Store){
		"edited line": func(s *Store) {
			data, _ := os.ReadFile(s.index.path())
			_ = os.WriteFile(s.index.path(), bytes.Replace(data, []byte("tester"), []byte("someone"), 1), 0o600)
		},
		"missing head": func(s *Store) { _ = os.Remove(s.index.headPath()) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := openTest(t, dir)
			record(t, s, "s-1", "one")
			_ = s.Close()
			s2 := openTest(t, dir)
			damage(s2)
			if err := s2.CreateSession(t.Context(), store.SessionRecord{ID: "s-2"}); !errors.Is(err, ErrIndexDamaged) {
				t.Fatalf("append onto a damaged index: %v", err)
			}
		})
	}
}

// A record that fails verify is not exported as sound: the export is
// refused, or with the option marked unverified, and either way VerifyFile
// on the copy fails. The trailer is the stored head, not the lines' own.
func TestExportOfTamperedDoesNotVerify(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one", "two", "three")
	_ = s.Close()
	s2 := openTest(t, dir)
	cutLines(t, s2.Path("s-1"), 1, "")
	mustFail(t, s2, "cut")
	var b bytes.Buffer
	if _, err := s2.Export("s-1", &b, ExportOptions{}); !errors.Is(err, ErrUnverified) || b.Len() != 0 {
		t.Fatalf("a failing record was exported: %v (%d bytes)", err, b.Len())
	}
	if _, err := s2.Export("s-1", &b, ExportOptions{Unverified: true}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "e.jsonl")
	_ = os.WriteFile(out, b.Bytes(), 0o600)
	if er, _ := VerifyFile(out); er.OK {
		t.Fatalf("the export of a failing record verifies: %+v", er)
	}
	// Even with the verified mark removed, the stored head still disagrees.
	lines := bytes.Split(bytes.TrimSuffix(b.Bytes(), []byte("\n")), []byte("\n"))
	var tr exportTrailer
	_ = json.Unmarshal(lines[len(lines)-1], &tr)
	tr.Verified, tr.Unverified = nil, ""
	last, _ := encode(tr)
	_ = os.WriteFile(out, append(bytes.Join(append(lines[:len(lines)-1], last), []byte("\n")), '\n'), 0o600)
	if er, _ := VerifyFile(out); er.OK {
		t.Fatalf("the stored head did not show the cut: %+v", er)
	}
}

// The review's probe: a head saying zero lines, with lines cut, passed with
// a note. A head that is not exactly as written is no head, and fails.
func TestInvalidHeadsFail(t *testing.T) {
	for name, head := range map[string]string{
		"zero":      `{"lines":0,"seq":0,"hash":""}`,
		"negative":  `{"lines":-2,"seq":1,"hash":"` + Genesis + `"}`,
		"uppercase": `{"LINES":1,"seq":1,"hash":"` + Genesis + `"}`,
		"extra":     `{"lines":1,"seq":1,"hash":"` + Genesis + `","x":1}`,
		"bad hash":  `{"lines":1,"seq":1,"hash":"nothex"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := openTest(t, dir)
			record(t, s, "s-1", "one", "two", "three")
			_ = s.Close()
			s2 := openTest(t, dir)
			cutLines(t, s2.Path("s-1"), 1, "")
			_ = os.WriteFile(s2.headPath("s-1"), []byte(head), 0o600)
			mustFail(t, s2, name)
			if err := s2.Acquire("s-1"); !errors.Is(err, ErrUnverified) {
				t.Fatalf("opened with a %s head: %v", name, err)
			}
		})
	}
}

// The review's probe: a session file deleted, then pruned. The prune does
// not make an empty file to prune; its tombstone says the file was missing
// and keeps the head the index recorded, not a clean zero.
func TestPruneOfAMissingSessionSaysSo(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	rec := record(t, s, "s-1", "one", "two")
	_, _ = rec.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted})
	_ = s.Close()
	s2 := openTest(t, dir)
	_ = os.Remove(s2.Path("s-1"))
	_ = os.Remove(s2.headPath("s-1"))
	p, err := s2.Prune("s-1", "managed", "retention")
	if err != nil || len(p) != 1 || !p[0].Missing || p[0].Verified || p[0].Head.Lines != 3 {
		t.Fatalf("prune: %+v %v", p, err)
	}
	if _, err := os.Stat(s2.Path("s-1")); err == nil {
		t.Fatal("the prune made a file")
	}
	ls, _ := s2.index.lines()
	last := ls[len(ls)-1]
	if last.Op != opPrune || !last.Missing || last.Verified == nil || *last.Verified || last.HeadLines != 3 || last.Unverified == "" {
		t.Fatalf("tombstone: %+v", last)
	}
}

// A damaged record can still be pruned, as found; the tombstone says it
// did not verify.
func TestPruneOfAnUnverifiedSession(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one", "two", "three")
	_ = s.Close()
	s2 := openTest(t, dir)
	cutLines(t, s2.Path("s-1"), 1, "")
	p, err := s2.Prune("s-1", "user", "by hand")
	if err != nil || len(p) != 1 || p[0].Verified || p[0].Missing || p[0].Head.Lines != 3 {
		t.Fatalf("prune: %+v %v", p, err)
	}
}

// The review's probes A and B: a listed session's file deleted, with or
// without its head. Opening it for writing refuses and makes no file, so
// verify keeps reporting it missing.
func TestDeletedSessionFileIsNotRecreated(t *testing.T) {
	for _, withHead := range []bool{false, true} {
		dir := t.TempDir()
		s := openTest(t, dir)
		record(t, s, "s-1", "one", "two")
		_ = s.Close()
		s2 := openTest(t, dir)
		_ = os.Remove(s2.Path("s-1"))
		if !withHead {
			_ = os.Remove(s2.headPath("s-1"))
		}
		if err := s2.Acquire("s-1"); !errors.Is(err, ErrUnverified) {
			t.Fatalf("head kept %v: Acquire: %v", withHead, err)
		}
		if _, err := s2.ClaimResume(t.Context(), "s-1"); !errors.Is(err, ErrUnverified) {
			t.Fatalf("head kept %v: ClaimResume: %v", withHead, err)
		}
		rec := agent.NewRecorder(s2, "s-1", "")
		if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "x"}); err == nil {
			t.Fatalf("head kept %v: an append made the session again", withHead)
		}
		if _, err := os.Stat(s2.Path("s-1")); err == nil {
			t.Fatalf("head kept %v: a file was made", withHead)
		}
		mustFail(t, s2, "after open")
		if err := s2.CreateSession(t.Context(), store.SessionRecord{ID: "s-1"}); err == nil {
			t.Fatal("a listed id was created again")
		}
	}
}

// The review's probe C: a record that fails only through the index (cut
// behind a head rewritten to match) is exported marked unverified; the
// copy fails on the mark, and with the mark stripped it still fails.
func TestUnverifiedMarkIsRequired(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	rec := record(t, s, "s-1", "one", "two")
	_, _ = rec.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted})
	_ = s.Close()
	s2 := openTest(t, dir)
	cutLines(t, s2.Path("s-1"), 2, "")
	data, _ := os.ReadFile(s2.Path("s-1"))
	var l line
	_ = json.Unmarshal(bytes.Split(data, []byte("\n"))[1], &l)
	_ = s2.writeHead("s-1", Head{Lines: 2, Seq: l.Seq, Hash: l.Hash})
	mustFail(t, s2, "cut behind a matching head")
	var b bytes.Buffer
	if _, err := s2.Export("s-1", &b, ExportOptions{Unverified: true}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "e.jsonl")
	_ = os.WriteFile(out, b.Bytes(), 0o600)
	if er, _ := VerifyFile(out); er.OK {
		t.Fatalf("the marked export verifies: %+v", er)
	}
	lines := bytes.Split(bytes.TrimSuffix(b.Bytes(), []byte("\n")), []byte("\n"))
	var tr exportTrailer
	_ = json.Unmarshal(lines[len(lines)-1], &tr)
	tr.Verified, tr.Unverified = nil, ""
	last, _ := encode(tr)
	_ = os.WriteFile(out, append(bytes.Join(append(lines[:len(lines)-1], last), []byte("\n")), '\n'), 0o600)
	if er, _ := VerifyFile(out); er.OK {
		t.Fatalf("the export with its mark stripped verifies: %+v", er)
	}
}

// A crash between a record's first line and its first real head leaves the
// sentinel head it was created with: that is a head behind the lines, noted,
// and writing goes on. (The sentinel is on disk before any line.)
func TestSentinelHeadCoversTheFirstWrite(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	if err := s.CreateSession(t.Context(), store.SessionRecord{ID: "s-1"}); err != nil {
		t.Fatal(err)
	}
	if h, ok := s.readHead("s-1"); !ok || h != sentinelHead {
		t.Fatalf("no sentinel head at creation: %+v %v", h, ok)
	}
	if h, ok := readHeadFile(s.index.headPath()); !ok || h.Lines == 0 {
		t.Fatalf("index head: %+v %v", h, ok)
	}
	rec := agent.NewRecorder(s, "s-1", "")
	_, _ = rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "one"})
	_ = s.Close()
	s2 := openTest(t, dir)
	_ = s2.writeHead("s-1", sentinelHead) // as a crash before the first real head leaves it
	if rep, _ := s2.Verify("s-1"); !rep.OK || len(rep.Notes) == 0 {
		t.Fatalf("sentinel behind one line: %+v", rep)
	}
	if err := s2.Acquire("s-1"); err != nil {
		t.Fatalf("refused: %v", err)
	}
}

// The review's probe R3-A: a live session cut to one line with its head
// deleted fails, is refused for writing, and stays failing.
func TestCutToOneLineWithoutAHeadFails(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one", "two", "three", "four")
	_ = s.Close()
	s2 := openTest(t, dir)
	cutLines(t, s2.Path("s-1"), 1, "")
	_ = os.Remove(s2.headPath("s-1"))
	mustFail(t, s2, "cut to one line, head deleted")
	if err := s2.Acquire("s-1"); !errors.Is(err, ErrUnverified) {
		t.Fatalf("opened: %v", err)
	}
	mustFail(t, s2, "after a refused open")
}

// The review's probe R3-B: the index cut to one line and its head deleted.
// The index fails, the next session is refused, and the sessions it no
// longer lists are reported by VerifyAll.
func TestIndexCutToOneLineWithoutAHeadFails(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one")
	record(t, s, "s-2", "two")
	record(t, s, "s-3", "three")
	_ = s.Close()
	s2 := openTest(t, dir)
	cutLines(t, s2.index.path(), 1, "")
	_ = os.Remove(s2.index.headPath())
	idx, reps, err := s2.VerifyAll()
	if err != nil || idx.OK {
		t.Fatalf("the cut index verifies: %+v %v", idx, err)
	}
	unlisted := 0
	for _, r := range reps {
		if !r.OK && strings.Contains(r.Reason, "not in its index") {
			unlisted++
		}
	}
	if unlisted != 2 {
		t.Fatalf("%d unlisted sessions reported, want 2: %+v", unlisted, reps)
	}
	if err := s2.CreateSession(t.Context(), store.SessionRecord{ID: "s-4"}); !errors.Is(err, ErrIndexDamaged) {
		t.Fatalf("an append onto the cut index: %v", err)
	}
}

// The review's probe E: a line the head counts, cut to a fragment. Verify
// and open give the same reason, and verify does not promise a repair.
func TestCountedLineCutShort(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one", "two", "three")
	_ = s.Close()
	s2 := openTest(t, dir)
	cutLines(t, s2.Path("s-1"), 2, `{"seq":3,"id":"x`)
	rep, _ := s2.Verify("s-1")
	if rep.OK || rep.Reason != cutShort {
		t.Fatalf("verify: %+v", rep)
	}
	for _, n := range rep.Notes {
		if strings.Contains(n, "cuts it off") {
			t.Fatalf("verify promises a repair: %q", n)
		}
	}
	if err := s2.Acquire("s-1"); err == nil || !strings.Contains(err.Error(), cutShort) {
		t.Fatalf("open: %v", err)
	}
}

// The review's round-4 probes: the creation head written over a real one.
// With more than one line left, that is damage, never a first-write crash,
// for a session and for the index.
func TestSentinelOverARealHeadFails(t *testing.T) {
	for _, keep := range []int{4, 2} {
		dir := t.TempDir()
		s := openTest(t, dir)
		record(t, s, "s-1", "one", "two", "three", "four")
		_ = s.Close()
		s2 := openTest(t, dir)
		cutLines(t, s2.Path("s-1"), keep, "")
		_ = s2.writeHead("s-1", sentinelHead)
		rep, _ := s2.Verify("s-1")
		if rep.OK || rep.Reason != sentinelBehind {
			t.Fatalf("session, %d lines: %+v", keep, rep)
		}
		if err := s2.Acquire("s-1"); !errors.Is(err, ErrUnverified) {
			t.Fatalf("session, %d lines, opened: %v", keep, err)
		}
	}
	for _, keep := range []int{4, 2} {
		dir := t.TempDir()
		s := openTest(t, dir)
		record(t, s, "s-1", "one")
		record(t, s, "s-2", "two")
		_ = s.Close()
		s2 := openTest(t, dir)
		cutLines(t, s2.index.path(), keep, "")
		_ = writeHeadFile(s2.index.headPath(), sentinelHead)
		if rep, _ := s2.VerifyIndex(); rep.OK || rep.Reason != sentinelBehind {
			t.Fatalf("index, %d lines: %+v", keep, rep)
		}
		if err := s2.CreateSession(t.Context(), store.SessionRecord{ID: "s-9"}); !errors.Is(err, ErrIndexDamaged) {
			t.Fatalf("index, %d lines, appended: %v", keep, err)
		}
	}
}

// A new index has its sentinel head on disk before its first line.
func TestIndexSentinelIsWrittenFirst(t *testing.T) {
	s := openTest(t, t.TempDir())
	var seen Head
	var ok bool
	afterIndexSentinel = func() { seen, ok = readHeadFile(s.index.headPath()) }
	defer func() { afterIndexSentinel = func() {} }()
	if err := s.CreateSession(t.Context(), store.SessionRecord{ID: "s-1"}); err != nil {
		t.Fatal(err)
	}
	if !ok || seen != sentinelHead {
		t.Fatalf("index head before the first line: %+v %v", seen, ok)
	}
	if _, err := os.Stat(s.index.path()); err != nil {
		t.Fatal(err)
	}
}

// Only the exact creation head may count no lines.
func TestOnlyTheExactSentinelCountsNoLines(t *testing.T) {
	if h, ok := parseHead([]byte(`{"lines":0,"seq":0,"hash":"` + Genesis + `"}`)); !ok || h != sentinelHead {
		t.Fatal("the sentinel was refused")
	}
	other := strings.Replace(Genesis, "0", "1", 1)
	for _, head := range []string{
		`{"lines":0,"seq":1,"hash":"` + Genesis + `"}`,
		`{"lines":0,"seq":0,"hash":"` + other + `"}`,
	} {
		if _, ok := parseHead([]byte(head)); ok {
			t.Errorf("accepted %s", head)
		}
	}
}

// If a session's first real head cannot be written, nothing more is written
// to it, so a second line never stands behind the creation head.
func TestFirstHeadFailurePoisons(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs a folder the process cannot write")
	}
	s := openTest(t, t.TempDir())
	if err := s.CreateSession(t.Context(), store.SessionRecord{ID: "s-1"}); err != nil {
		t.Fatal(err)
	}
	headDir := filepath.Dir(s.headPath("s-1"))
	_ = os.Chmod(headDir, 0o500)
	defer func() { _ = os.Chmod(headDir, 0o700) }()
	rec := agent.NewRecorder(s, "s-1", "")
	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "one"}); err == nil {
		t.Fatal("the first head's failure was not reported")
	}
	_ = os.Chmod(headDir, 0o700)
	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "two"}); err == nil {
		t.Fatal("a second line was written behind the creation head")
	}
	if rep, _ := s.Verify("s-1"); !rep.OK || rep.Events != 1 {
		t.Fatalf("after: %+v", rep)
	}
}
