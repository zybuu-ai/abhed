package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/recordship"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// A session recorded under an org policy with record.ship on reaches the org
// server, line for line, with the sign-in token, by the time the record closes.
func TestRecordShipsToTheOrgServer(t *testing.T) {
	srv := newOrgServer(t)
	srv.policy = `{"record":{"ship":{"enabled":true}}}`
	var mu sync.Mutex
	held := map[string][][]byte{}
	var auth []string
	inner := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, recordship.LinesPath) {
			inner.ServeHTTP(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		auth = append(auth, r.Header.Get("Authorization"))
		if r.URL.Path == recordship.EventsPath {
			return
		}
		h, raws, err := recordship.DecodeBatch(r.Body, recordship.MaxBatchBytes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		segs, err := local.CheckSegment(raws, h.Session, h.Before)
		if err != nil || h.From != int64(len(held[h.Session])) {
			http.Error(w, "refused", http.StatusUnprocessableEntity)
			return
		}
		held[h.Session] = append(held[h.Session], raws...)
		_ = json.NewEncoder(w).Encode(recordship.Ack{Lines: int64(len(held[h.Session])), Hash: segs[len(segs)-1].Hash})
	})
	pf, p := orgClient(t, srv)
	var out, errOut bytes.Buffer
	if err := enrollOrgPolicy(context.Background(), pf, "org", p, nil, &out, &errOut); err != nil {
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
	for _, m := range []string{"one", "two"} {
		if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: m}); err != nil {
			t.Fatal(err)
		}
	}
	path := rec.Path("s-ship")
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	mu.Lock()
	defer mu.Unlock()
	if len(held["s-ship"]) != len(want) {
		t.Fatalf("the server holds %d lines, the record %d", len(held["s-ship"]), len(want))
	}
	for i := range want {
		if !bytes.Equal(held["s-ship"][i], want[i]) {
			t.Fatalf("line %d differs", i+1)
		}
	}
	for _, a := range auth {
		if a != "Bearer tok-1" {
			t.Errorf("shipped without the sign-in token: %q", a)
		}
	}
}
