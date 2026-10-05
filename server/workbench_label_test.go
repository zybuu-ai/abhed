package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A session opened in the workbench is listed by its first message once one
// is sent, on the memory store as on a durable one.
func TestWorkbenchSessionListedByItsFirstMessage(t *testing.T) {
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Store: agent.NewMemStore()})
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"workbench":true}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	for _, text := range []string{"Fix the retry loop", "and the tests"} {
		msg := httptest.NewRecorder()
		h.ServeHTTP(msg, httptest.NewRequest("POST", "/v1/sessions/"+created.ID+"/messages",
			strings.NewReader(`{"prompt":"`+text+`"}`)))
		if msg.Code != http.StatusAccepted {
			t.Fatalf("message: %d %s", msg.Code, msg.Body)
		}
		waitState(t, s, created.ID, "done")
	}
	list := httptest.NewRecorder()
	h.ServeHTTP(list, httptest.NewRequest("GET", "/v1/sessions", nil))
	var got []sessionSummary
	_ = json.Unmarshal(list.Body.Bytes(), &got)
	if len(got) != 1 || got[0].Prompt != "Fix the retry loop" {
		t.Fatalf("listed as %+v, want the first message", got)
	}
}
