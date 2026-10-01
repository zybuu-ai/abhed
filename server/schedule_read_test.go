package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// An administrator may read a scheduled run, which no identity owns, and each
// read is audited; anyone else is told it does not exist, and no one may
// continue it.
func TestAdminReadsAScheduledRun(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
	st.rows["s-sched"] = store.SessionRecord{ID: "s-sched", Tenant: "default", User: "schedule:nightly"}
	st.rows["s-other"] = store.SessionRecord{ID: "s-other", Tenant: "other", User: "schedule:nightly"}
	for _, id := range []string{"s-sched", "s-other"} {
		if err := st.Append(agent.Event{SessionID: id, Seq: 1, Type: agent.EvUserMessage, Payload: []byte(`{"text":"hi"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	var audited []string
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Store: st,
		AdminAudit: func(_ context.Context, action, target string, _ map[string]any) {
			audited = append(audited, action+" "+target)
		}})
	req := func(id string, groups ...string) *http.Request {
		r := httptest.NewRequest("GET", "/v1/sessions/"+id+"/replay", nil)
		r.SetPathValue("id", id)
		r = asUser(r, "default", "local:ann")
		return r.WithContext(auth.WithIdentity(r.Context(), &auth.Identity{Subject: "ann", Tenant: "default", Groups: groups}))
	}
	for _, c := range []struct {
		name   string
		r      *http.Request
		status int
	}{
		{"admin", req("s-sched", DefaultAdminGroup), http.StatusOK},
		{"not an admin", req("s-sched"), http.StatusNotFound},
		{"admin, another tenant", req("s-other", DefaultAdminGroup), http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		s.replaySession(w, c.r)
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.status)
		}
	}
	if len(audited) != 1 || audited[0] != "session.read s-sched" {
		t.Errorf("audited %v, want one read of s-sched", audited)
	}
	if s.mayAccess(req("s-sched", DefaultAdminGroup), "s-sched") {
		t.Error("an administrator may continue a scheduled run")
	}
}
