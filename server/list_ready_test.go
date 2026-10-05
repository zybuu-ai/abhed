package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// rowsMem is a memory store that keeps session rows, and calls written once
// each row is written, before the server has gone on to start the session.
type rowsMem struct {
	*agent.MemStore
	mu      sync.Mutex
	rows    []store.SessionRecord
	written func(id string)
}

func (r *rowsMem) CreateSession(_ context.Context, s store.SessionRecord) error {
	r.mu.Lock()
	r.rows = append(r.rows, s)
	hook := r.written
	r.mu.Unlock()
	if hook != nil && s.ParentID == "" {
		hook(s.ID)
	}
	return nil
}

func (r *rowsMem) ListSessions(context.Context, int) ([]store.SessionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.SessionRecord(nil), r.rows...), nil
}

// A session the list names answers on its event stream at once: a page that
// opens the newest listed session the moment another tab creates it never
// gets a 404 for it, whether the session was started with a prompt or opened
// in the workbench.
func TestAListedSessionNeverAnswers404OnItsEvents(t *testing.T) {
	st := &rowsMem{MemStore: agent.NewMemStore()}
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Store: st})
	h := s.Handler()
	var bad []string
	check := func(when string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions", nil))
		var list []sessionSummary
		_ = json.Unmarshal(rec.Body.Bytes(), &list)
		for _, l := range list {
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			ev := httptest.NewRecorder()
			h.ServeHTTP(ev, httptest.NewRequest("GET", "/v1/sessions/"+l.ID+"/events", nil).WithContext(ctx))
			cancel()
			if ev.Code == http.StatusNotFound {
				bad = append(bad, when+": "+l.ID+" listed but /events answered 404")
			}
		}
	}
	st.written = func(string) { check("as its row is written") }
	for _, body := range []string{`{"prompt":"hi"}`, `{"workbench":true}`} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(body)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("create %s: %d %s", body, rec.Code, rec.Body)
		}
		check("after " + body)
	}
	if len(bad) > 0 {
		t.Fatal(strings.Join(bad, "\n"))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions", nil))
	var list []sessionSummary
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 2 {
		t.Fatalf("both sessions are listed once started: %s", rec.Body)
	}
}
