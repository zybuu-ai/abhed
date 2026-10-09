package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// fourEyes is a gate needing two approvals from approvers other than the
// session's owner; a refusal from any of them settles.
type fourEyes struct {
	mu        sync.Mutex
	approvers map[string]bool
	votes     map[string]map[string]bool
	seen      []ApprovalAnswer
	err       error
}

func (g *fourEyes) MayAnswer(_ context.Context, tenant, owner, user string) bool {
	return tenant == "default" && owner != user && g.approvers[user]
}

func (g *fourEyes) Answer(_ context.Context, a ApprovalAnswer) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen = append(g.seen, a)
	switch {
	case g.err != nil:
		return false, g.err
	case !a.Approved:
		return true, nil
	case a.By == a.Owner:
		return false, ErrAnswerRefused
	}
	if g.votes == nil {
		g.votes = map[string]map[string]bool{}
	}
	if g.votes[a.Request] == nil {
		g.votes[a.Request] = map[string]bool{}
	}
	g.votes[a.Request][a.By] = true
	return len(g.votes[a.Request]) >= 2, nil
}

type askGateRig struct {
	srv  *Server
	h    http.Handler
	gate *fourEyes
}

func newAskGateRig(t *testing.T, withGate bool) *askGateRig {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "oidc"
	mw := &auth.Middleware{PublicPaths: PublicPaths(), Verifier: tokens{
		"alice": {Subject: "alice", Tenant: "default"},
		"bob":   {Subject: "bob", Tenant: "default"},
		"carol": {Subject: "carol", Tenant: "default"},
		"dave":  {Subject: "dave", Tenant: "default"},
		"eve":   {Subject: "eve", Tenant: "other"},
	}}
	g := &fourEyes{approvers: map[string]bool{"bob": true, "carol": true, "eve": true}}
	opts := Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Auth: mw}
	if withGate {
		opts.ApprovalGate = g
	}
	srv := New(opts)
	return &askGateRig{srv: srv, h: srv.Handler(), gate: g}
}

func (r *askGateRig) send(token, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w
}

// session starts one of alice's sessions and has it ask about a call.
func (r *askGateRig) session(t *testing.T) (string, *liveSession, chan turnResult, *agent.Answer) {
	t.Helper()
	var created createResponse
	if w := r.send("alice", "/v1/sessions", `{"workbench":true}`); json.Unmarshal(w.Body.Bytes(), &created) != nil || created.SessionID == "" {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	live := r.srv.anyLive(t)
	ctx, answer := agent.ExpectAnswer(agent.WithRequestID(context.Background(), "ev-a"))
	c := make(chan turnResult, 1)
	go func() {
		ok, err := live.Approve(ctx, "web_fetch", json.RawMessage(`{"url":"https://example.com"}`),
			policy.Result{Decision: policy.Ask, Step: "ask", Reason: "matched ask rule web_fetch"})
		c <- turnResult{ok, err}
	}()
	<-waitAsked(live, "ev-a").ready
	return created.SessionID, live, c, answer
}

// Under the gate, the owner cannot approve their own request, one approver
// is not enough, and the second settles it in the turn, named as approver.
func TestApprovalGateHoldsUntilTwoApprovers(t *testing.T) {
	r := newAskGateRig(t, true)
	id, _, c, answer := r.session(t)
	approve := func(token string) *httptest.ResponseRecorder {
		return r.send(token, "/v1/sessions/"+id+"/approve", `{"approved":true,"request_id":"ev-a"}`)
	}
	if w := approve("alice"); w.Code != http.StatusForbidden {
		t.Fatalf("the owner approved their own request: %d %s", w.Code, w.Body)
	}
	if w := approve("dave"); w.Code != http.StatusNotFound {
		t.Fatalf("a non-approver reached the session: %d", w.Code)
	}
	if w := approve("eve"); w.Code != http.StatusNotFound {
		t.Fatalf("an approver in another tenant reached the session: %d", w.Code)
	}
	if w := approve("bob"); w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"settled":false`) {
		t.Fatalf("first approval: %d %s", w.Code, w.Body)
	}
	silent(t, c, "one approval of two")
	if w := approve("bob"); w.Code != http.StatusAccepted {
		t.Fatalf("a repeat by the same approver: %d", w.Code)
	}
	silent(t, c, "the same approver twice")
	if w := approve("carol"); w.Code != http.StatusNoContent {
		t.Fatalf("second approval: %d %s", w.Code, w.Body)
	}
	if res := <-c; !res.ok {
		t.Fatal("the turn did not take the settled approval")
	}
	if answer.Approver != "carol" {
		t.Fatalf("answer %+v", answer)
	}
	r.gate.mu.Lock()
	defer r.gate.mu.Unlock()
	for _, a := range r.gate.seen {
		if a.Request != "ev-a" || a.Owner != "alice" || a.SessionID != id || a.Tool != "web_fetch" || a.Tenant != "default" {
			t.Fatalf("the gate was told %+v", a)
		}
	}
}

// A refusal settles at once; a gate that fails leaves the request waiting.
func TestApprovalGateRefusalAndFailure(t *testing.T) {
	r := newAskGateRig(t, true)
	id, _, c, _ := r.session(t)
	r.gate.err = errors.New("votes unavailable")
	if w := r.send("bob", "/v1/sessions/"+id+"/approve", `{"approved":false,"request_id":"ev-a"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a failing gate: %d", w.Code)
	}
	silent(t, c, "a failed gate")
	r.gate.err = nil
	if w := r.send("bob", "/v1/sessions/"+id+"/approve", `{"approved":false,"request_id":"ev-a"}`); w.Code != http.StatusNoContent {
		t.Fatalf("refusal: %d %s", w.Code, w.Body)
	}
	if res := <-c; res.ok {
		t.Fatal("a refusal approved the call")
	}
}

// Without a gate, nobody but the owner reaches a session's approvals.
func TestWithoutAGateOnlyTheOwnerAnswers(t *testing.T) {
	r := newAskGateRig(t, false)
	id, _, c, _ := r.session(t)
	if w := r.send("bob", "/v1/sessions/"+id+"/approve", `{"approved":true,"request_id":"ev-a"}`); w.Code != http.StatusNotFound {
		t.Fatalf("another account answered: %d", w.Code)
	}
	if w := r.send("alice", "/v1/sessions/"+id+"/approve", `{"approved":true,"request_id":"ev-a"}`); w.Code != http.StatusNoContent {
		t.Fatalf("the owner: %d", w.Code)
	}
	if res := <-c; !res.ok {
		t.Fatal("the owner's approval was lost")
	}
}

// PendingAsks lists the waiting requests of the sessions it is asked about,
// with their arguments redacted, and nothing else.
func TestPendingAsks(t *testing.T) {
	r := newAskGateRig(t, true)
	id, live, c, _ := r.session(t)
	defer func() {
		_ = r.send("bob", "/v1/sessions/"+id+"/approve", `{"approved":false,"request_id":"ev-a"}`)
		<-c
	}()
	live.Loop.Recorder.Redact = redactWord{"example.com"}
	asks := r.srv.PendingAsks(func(tenant, owner string) bool { return tenant == "default" && owner == "alice" })
	if len(asks) != 1 || asks[0].SessionID != id || asks[0].Request != "ev-a" || asks[0].Tool != "web_fetch" || asks[0].Owner != "alice" {
		t.Fatalf("asks %+v", asks)
	}
	if strings.Contains(string(asks[0].Args), "example.com") || !strings.Contains(string(asks[0].Args), "[x]") {
		t.Fatalf("arguments not redacted: %s", asks[0].Args)
	}
	if asks := r.srv.PendingAsks(func(string, string) bool { return false }); len(asks) != 0 {
		t.Fatalf("listed what was excluded: %+v", asks)
	}
	live.Loop.Recorder.Redact = nil
	if asks := r.srv.PendingAsks(func(string, string) bool { return true }); string(asks[0].Args) != `{"withheld":true}` {
		t.Fatalf("arguments without a redactor: %s", asks[0].Args)
	}
}

type redactWord struct{ word string }

func (r redactWord) Redact(b []byte) []byte {
	return []byte(strings.ReplaceAll(string(b), r.word, "[x]"))
}
func (r redactWord) Span() int { return len(r.word) }

type oneSession struct {
	SessionRecorder
	rec store.SessionRecord
}

func (o oneSession) GetSession(_ context.Context, id string) (store.SessionRecord, error) {
	if id != o.rec.ID {
		return store.SessionRecord{}, store.ErrNotFound
	}
	return o.rec, nil
}

// A stored session's approvals are answered by its owner, or by whom the
// gate allows in its tenant.
func TestStoredAnswerer(t *testing.T) {
	r := newAskGateRig(t, true)
	r.srv.sessions = oneSession{rec: store.SessionRecord{ID: "s1", Tenant: "default", User: "alice"}}
	ask := func(user, tenant, id string) bool {
		req := httptest.NewRequest("POST", "/", nil)
		ctx := context.WithValue(req.Context(), ctxUser, user)
		ctx = context.WithValue(ctx, ctxTenant, tenant)
		_, owner, ok := r.srv.storedAnswerer(req.WithContext(ctx), id)
		return ok && owner == "alice"
	}
	for _, tc := range []struct {
		user, tenant, id string
		want             bool
	}{
		{"alice", "default", "s1", true},
		{"bob", "default", "s1", true},
		{"dave", "default", "s1", false},
		{"bob", "other", "s1", false},
		{"bob", "default", "s2", false},
	} {
		if got := ask(tc.user, tc.tenant, tc.id); got != tc.want {
			t.Fatalf("%+v: %v", tc, got)
		}
	}
}
