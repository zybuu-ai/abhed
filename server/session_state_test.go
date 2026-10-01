package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// listedAs fetches the session list as user and returns id's state and reason.
func listedAs(t *testing.T, s *Server, user, id string) (string, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	req.Header.Set("X-Abhed-User", user)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var list []sessionSummary
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	for _, l := range list {
		if l.ID == id {
			return l.State, l.Reason
		}
	}
	t.Fatalf("%s is not listed: %s", id, rec.Body)
	return "", ""
}

// A finished session is listed done with how its last run ended, so the
// console's pills for an interrupt, a shutdown or a deadline can show.
func TestSessionListNamesHowARunEnded(t *testing.T) {
	q := newQueueRig(t)
	if rec := q.do("alice", "POST", "/interrupt", ""); rec.Code >= 300 {
		t.Fatalf("interrupt = %d %s", rec.Code, rec.Body)
	}
	q.waitEnded()
	state, reason := "", ""
	for deadline := time.Now().Add(5 * time.Second); state != "done"; time.Sleep(20 * time.Millisecond) {
		if state, reason = listedAs(t, q.s, "alice", q.id); time.Now().After(deadline) {
			t.Fatalf("an interrupted session is listed %q", state)
		}
	}
	if reason != string(agent.TermUserInterrupt) {
		t.Fatalf("an interrupted session is listed done with reason %q", reason)
	}
}

// A session this process did not run is listed from its stored row, by the
// terminal reason recorded there.
func TestSessionListNamesARecordedEnd(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	ended := time.Now()
	for id, reason := range map[string]string{"s-drained": "shutdown", "s-late": "deadline", "s-stuck": "stalled", "s-old": ""} {
		st.rows[id] = store.SessionRecord{ID: id, Tenant: "acme", User: "alice", StartedAt: ended, EndedAt: &ended, TerminalReason: reason}
	}
	st.rows["s-now"] = store.SessionRecord{ID: "s-now", Tenant: "acme", User: "alice", StartedAt: ended}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Store: st})
	for id, want := range map[string][2]string{"s-drained": {"done", "shutdown"}, "s-late": {"done", "deadline"},
		"s-stuck": {"done", "stalled"}, "s-old": {"done", ""}, "s-now": {"running", ""}} {
		if state, reason := listedAs(t, s, "alice", id); state != want[0] || reason != want[1] {
			t.Errorf("%s is listed %q/%q, want %q/%q", id, state, reason, want[0], want[1])
		}
	}
}

// busyRows is a tenant's session rows, newest first, listed up to a limit as
// the durable store lists them, and found by id.
type busyRows []store.SessionRecord

func (busyRows) CreateSession(context.Context, store.SessionRecord) error { return nil }
func (b busyRows) ListSessions(_ context.Context, limit int) ([]store.SessionRecord, error) {
	if limit < len(b) {
		return b[:limit], nil
	}
	return b, nil
}
func (b busyRows) GetSession(_ context.Context, id string) (store.SessionRecord, error) {
	for _, r := range b {
		if r.ID == id {
			return r, nil
		}
	}
	return store.SessionRecord{}, store.ErrNotFound
}

// A session's state is answered to its owner alone, found by id however many
// newer sessions others in the tenant hold, and live here or only stored.
func TestSessionStateIsTheOwnersAndFoundById(t *testing.T) {
	var rows busyRows
	for i := 0; i < 250; i++ {
		rows = append(rows, store.SessionRecord{ID: fmt.Sprintf("s-other-%d", i), Tenant: "default", User: "omar"})
	}
	rows = append(rows, store.SessionRecord{ID: "s-mine", Tenant: "default", User: "priya"})
	s := &Server{sessions: rows, running: map[string]*liveSession{
		"s-live": {ID: "s-live", Tenant: "default", User: "priya", State: "waiting_approval", Turns: 3},
	}, log: discardLogger()}
	get := func(id, tenant, user string) (int, sessionStateResponse) {
		rec := httptest.NewRecorder()
		req := asUser(httptest.NewRequest("GET", "/v1/sessions/"+id+"/state", nil), tenant, user)
		req.SetPathValue("id", id)
		s.sessionState(rec, req)
		var out sessionStateResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, st := get("s-mine", "default", "priya"); code != http.StatusOK || st.State != "running" {
		t.Fatalf("the owner's stored session in a busy tenant: %d %+v", code, st)
	}
	if code, st := get("s-live", "default", "priya"); code != http.StatusOK || st.State != "waiting_approval" || st.Turns != 3 {
		t.Fatalf("the owner's live session: %d %+v", code, st)
	}
	for _, c := range []struct{ id, tenant, user string }{
		{"s-mine", "default", "omar"}, {"s-mine", "acme", "priya"}, {"s-live", "default", "omar"}, {"s-none", "default", "priya"},
	} {
		if code, st := get(c.id, c.tenant, c.user); code != http.StatusNotFound || st.State != "" {
			t.Errorf("%s as %s/%s: %d %+v, want 404 and nothing", c.id, c.tenant, c.user, code, st)
		}
	}
}
