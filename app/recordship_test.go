package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/recordship"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// fakeIntake answers the record intake as the org server does: it checks
// each batch and keeps it only where it follows what it holds, and otherwise
// answers 409 with where it is, so a client that lost an answer goes on.
type fakeIntake struct {
	mu   sync.Mutex
	held map[string][][]byte
	auth []string
	// slow, when set, delays the answer to the first batch after keeping it.
	slow time.Duration
}

func (f *fakeIntake) head(id string) (int64, string) {
	ls := f.held[id]
	if len(ls) == 0 {
		return 0, local.Genesis
	}
	segs, _ := local.CheckSegment(ls[len(ls)-1:], id, prevHash(ls[len(ls)-1]))
	return int64(len(ls)), segs[0].Hash
}

func prevHash(raw []byte) string {
	var l struct{ Prev string }
	_ = json.Unmarshal(raw, &l)
	return l.Prev
}

func (f *fakeIntake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if r.URL.Path == recordship.EventsPath {
		f.mu.Unlock()
		return
	}
	h, raws, err := recordship.DecodeBatch(r.Body, recordship.MaxBodyBytes)
	if err != nil {
		f.mu.Unlock()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	n, hash := f.head(h.Session)
	if h.From != n || h.Before != hash {
		f.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(recordship.Ack{Lines: n, Hash: hash, Error: "not where the server is"})
		return
	}
	segs, err := local.CheckSegment(raws, h.Session, h.Before)
	if err != nil {
		f.mu.Unlock()
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(recordship.Ack{Error: err.Error()})
		return
	}
	first := n == 0
	f.held[h.Session] = append(f.held[h.Session], raws...)
	n = int64(len(f.held[h.Session]))
	slow := f.slow
	f.mu.Unlock()
	if first && slow > 0 {
		time.Sleep(slow) // the answer is late; the batch is already kept
	}
	_ = json.NewEncoder(w).Encode(recordship.Ack{Lines: n, Hash: segs[len(segs)-1].Hash})
}

// shipThroughRecord enrols, records texts through openRecord with a pause
// after each, closes the record and checks the server holds every line.
func shipThroughRecord(t *testing.T, intake *fakeIntake, pause time.Duration, texts ...string) {
	t.Helper()
	srv := newOrgServer(t)
	srv.policy = `{"record":{"ship":{"enabled":true}}}`
	intake.held = map[string][][]byte{}
	srv.mu.Lock()
	srv.records = intake
	srv.mu.Unlock()
	pf, p := orgClient(t, srv)
	var out, errOut bytes.Buffer
	if err := enrollOrgPolicy(context.Background(), pf, "org", p, enrolOptions{accept: true}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	cfg := loadOrg(t)
	if !cfg.Record.Ship.Enabled || !cfg.ManagedSets("record.ship") {
		t.Fatalf("record.ship not on: %+v", cfg.Record.Ship)
	}
	cfg.Record.Dir = filepath.Join(t.TempDir(), "records")
	rec, err := openRecord(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.CreateSession(context.Background(), store.SessionRecord{ID: "s-ship", User: "alice", Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	r := agent.NewRecorder(rec, "s-ship", "")
	for _, m := range texts {
		if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: m}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(pause)
	}
	path := rec.Path("s-ship")
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	intake.mu.Lock()
	defer intake.mu.Unlock()
	got := intake.held["s-ship"]
	if len(got) != len(want) {
		t.Fatalf("the server holds %d lines, the record %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("line %d differs", i+1)
		}
	}
	for _, a := range intake.auth {
		if a != "Bearer tok-1" {
			t.Errorf("shipped without the sign-in token: %q", a)
		}
	}
}

// A session recorded under an org policy with record.ship on reaches the org
// server, line for line, with the sign-in token, by the time the record closes.
func TestRecordShipsToTheOrgServer(t *testing.T) {
	shipThroughRecord(t, &fakeIntake{}, 0, "one", "two")
}

// Closing the record while a pass waits for its answer cancels that pass;
// the server kept the batch, so the last pass is told where it is (409) and
// sends only the rest. The test server once refused that instead, which
// left the session short when a pass happened to be in flight at close.
func TestRecordShipsAfterAnAnswerLostAtClose(t *testing.T) {
	shipThroughRecord(t, &fakeIntake{slow: 300 * time.Millisecond}, 50*time.Millisecond, "one", "two")
}
