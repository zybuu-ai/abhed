package server

import (
	"encoding/json"
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
