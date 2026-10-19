package ocsf_test

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/ocsf"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// canary redacts one stored secret, as the vault's redactor does.
type canary struct{}

func (canary) Redact(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("sk-CANARY-4242"), []byte("[REDACTED:api_key]"))
}
func (canary) Span() int { return len("sk-CANARY-4242") }

// recordWithSink opens a record whose lines feed a sink, as the CLI does.
func recordWithSink(t *testing.T, dir, path string) (*local.Store, *ocsf.Sink) {
	t.Helper()
	var sink *ocsf.Sink
	rec, err := local.Open(local.Options{Dir: dir, User: "tester", Redact: canary{},
		OnAppend: func(l local.Sealed) {
			if sink != nil {
				sink.Add(ocsf.Line{Event: l.Event, Prev: l.Prev, Hash: l.Hash})
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	sink, err = ocsf.OpenSink(ocsf.SinkOptions{Path: path, Map: ocsf.Options{Hostname: "h", Tenant: rec.Tenant()},
		Read: func(id string, after int64) ([]ocsf.Line, error) {
			sealed, err := rec.SealedSince(id, after)
			out := make([]ocsf.Line, len(sealed))
			for i, s := range sealed {
				out[i] = ocsf.Line{Event: s.Event, Prev: s.Prev, Hash: s.Hash}
			}
			return out, err
		},
		Sessions: func() (map[string]int64, error) {
			ids, err := rec.SessionIDs()
			out := map[string]int64{}
			for _, id := range ids {
				if seq, _, err := rec.Index().Head(id); err == nil {
					out[id] = seq
				}
			}
			return out, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	return rec, sink
}

func newSession(t *testing.T, rec *local.Store, id string) *agent.Recorder {
	t.Helper()
	if err := rec.CreateSession(context.Background(), store.SessionRecord{ID: id, User: "tester", Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	return agent.NewRecorder(rec, id, "")
}

func readEvents(t *testing.T, path string) []ocsf.Line {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []ocsf.Line
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		l, err := ocsf.ReplayJSON(sc.Bytes())
		if err != nil {
			t.Fatalf("%v: %s", err, sc.Text())
		}
		out = append(out, l)
	}
	return out
}

// The file is derived from the chain: every line comes back from it with a
// hash the record's own check accepts, a stored secret redacted as in the
// record, and the cursor carries over a restart without a repeat or a gap.
func TestSinkIsTheRecordAndSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "ocsf", "events.jsonl")

	rec, sink := recordWithSink(t, dir, path)
	r := newSession(t, rec, "s-1")
	for _, text := range []string{"first", "use sk-CANARY-4242 here", "third"} {
		if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	// Written while no sink runs: caught up when the next one starts.
	if _, err := r.Record(agent.EvAgentMessage, agent.ActorAgent, agent.Trusted, agent.Message{Text: "while off"}); err != nil {
		t.Fatal(err)
	}
	_ = rec.Close()

	rec, sink = recordWithSink(t, dir, path)
	r = agent.NewRecorder(rec, "s-1", "")
	r.Advance(4)
	if _, err := r.Record(agent.EvAgentMessage, agent.ActorAgent, agent.Trusted, agent.Message{Text: "after"}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("sk-CANARY-4242")) {
		t.Fatal("the OCSF file holds a secret the record redacted")
	}
	if !bytes.Contains(data, []byte("[REDACTED:api_key]")) {
		t.Fatal("the redacted message is not in the file")
	}
	got := readEvents(t, path)
	sealed, err := rec.SealedSince("s-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(sealed) {
		t.Fatalf("the file has %d events, the record %d lines", len(got), len(sealed))
	}
	prev := local.Genesis
	for i, l := range got {
		if l.Seq != sealed[i].Seq || l.ID != sealed[i].ID {
			t.Fatalf("event %d is seq %d, the record's line seq %d", i, l.Seq, sealed[i].Seq)
		}
		if l.Prev != prev {
			t.Fatalf("seq %d: prev %s, want %s", l.Seq, l.Prev, prev)
		}
		h, err := local.LineHash(l.Event, l.Prev)
		if err != nil || h != l.Hash || h != sealed[i].Hash {
			t.Fatalf("seq %d: the replayed line hashes to %s, the record says %s (%v)", l.Seq, h, sealed[i].Hash, err)
		}
		prev = l.Hash
	}
}

// Turning the sink on does not write the history before it: that is what
// `abhed logs` is for.
func TestSinkStartsAtThePresentEnd(t *testing.T) {
	dir := t.TempDir()
	rec, err := local.Open(local.Options{Dir: dir, User: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	r := newSession(t, rec, "s-old")
	if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "old"}); err != nil {
		t.Fatal(err)
	}
	_ = rec.Close()

	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, sink := recordWithSink(t, dir, path)
	r = newSession(t, rec, "s-new")
	if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "new"}); err != nil {
		t.Fatal(err)
	}
	_ = sink.Close()
	_ = rec.Close()
	for _, l := range readEvents(t, path) {
		if l.SessionID == "s-old" {
			t.Fatal("the sink wrote a session's history from before it was turned on")
		}
	}
	if n := len(readEvents(t, path)); n == 0 {
		t.Fatal("the new session was not written")
	}
}

func TestSinkRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, err := local.Open(local.Options{Dir: dir, User: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	sink, err := ocsf.OpenSink(ocsf.SinkOptions{Path: path, MaxBytes: 2048, Keep: 2, Version: "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	r := newSession(t, rec, "s-1")
	for i := 0; i < 40; i++ {
		ev, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: strings.Repeat("x", 100)})
		if err != nil {
			t.Fatal(err)
		}
		sink.Add(ocsf.Line{Event: ev})
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + ".1", path + ".2"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s is %v", p, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatal("more rotated files kept than asked")
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte(`"version":"1.1.0"`)) {
		t.Fatal("the sink did not write the version asked for")
	}
}

// Lines the queue had no room for are read back from the record: the file
// still holds each line once, in the record's order.
func TestSinkCatchesUpWhatTheQueueDropped(t *testing.T) {
	defer ocsf.SetSinkQueue(1)()
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, sink := recordWithSink(t, dir, path)
	r := newSession(t, rec, "s-1")
	for i := 0; i < 300; i++ {
		if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	got := readEvents(t, path)
	sealed, _ := rec.SealedSince("s-1", 0)
	if len(got) != len(sealed) {
		t.Fatalf("the file has %d events, the record %d lines", len(got), len(sealed))
	}
	for i := range got {
		if got[i].Seq != sealed[i].Seq {
			t.Fatalf("event %d is seq %d, want %d", i, got[i].Seq, sealed[i].Seq)
		}
	}
}
