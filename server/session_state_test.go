package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zybuu-ai/abhed/store"
)

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
		"s-live": {ID: "s-live", Tenant: "default", User: "priya", State: "waiting_approval"},
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
	if code, st := get("s-live", "default", "priya"); code != http.StatusOK || st.State != "waiting_approval" {
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
