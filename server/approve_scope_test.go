package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// An ask rule asks every time: a scope the session remembers does not
// satisfy it, and the card offers none to remember.
func TestAskRuleIgnoresARememberedScope(t *testing.T) {
	h, live, _, id := approvalSession(t, false)
	live.mu.Lock()
	live.allowed["web_search"] = true
	live.mu.Unlock()
	c := make(chan turnResult, 1)
	go func() {
		ok, err := live.Approve(agent.WithRequestID(context.Background(), "ev-a"), "web_search", nil,
			policy.Result{Decision: policy.Ask, Step: "ask", Scope: "web_search", Reason: "matched ask rule web_search"})
		c <- turnResult{ok, err}
	}()
	// A remembered scope that satisfied the rule would return without asking.
	var p *pendingApproval
	for deadline := time.Now().Add(5 * time.Second); p == nil; {
		select {
		case res := <-c:
			t.Fatalf("the turn was answered without asking: %+v", res)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the request was never published")
		}
		live.mu.Lock()
		p = live.pending
		live.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	<-p.ready
	if p.Scope != "" {
		t.Fatalf("the card offers %q", p.Scope)
	}
	if code := approve(h, id, `{"approved":true,"request_id":"ev-a","scope":"web_search"}`); code != http.StatusBadRequest {
		t.Fatalf("always allow on an ask rule = %d, want 400", code)
	}
	if code := approve(h, id, `{"approved":false,"request_id":"ev-a"}`); code != http.StatusNoContent {
		t.Fatalf("reject = %d", code)
	}
	if res := <-c; res.ok {
		t.Fatal("the ask rule was satisfied by a remembered scope")
	}
}

// The answer names the signed-in person who gave it, and the scope they
// chose to always allow, whether they approve or refuse.
func TestTheAnswerNamesTheApprover(t *testing.T) {
	for _, approved := range []bool{true, false} {
		s := testServer(t)
		h := s.Handler()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"workbench":true}`)))
		live := s.anyLive(t)
		res := policy.Result{Decision: policy.Ask, Step: "default", Scope: "bash(mkdir *)"}
		ctx, answer := agent.ExpectAnswer(agent.WithRequestID(context.Background(), "ev-a"))
		c := make(chan turnResult, 1)
		go func() {
			ok, err := live.Approve(ctx, "bash", nil, res)
			c <- turnResult{ok, err}
		}()
		<-waitAsked(live, "ev-a").ready

		r := httptest.NewRequest("POST", "/", nil)
		rctx := auth.WithIdentity(r.Context(), &auth.Identity{Subject: "olga", Email: "olga@example.com"})
		r = r.WithContext(context.WithValue(rctx, ctxUser, "olga@example.com"))
		scope := ""
		if approved {
			scope = res.Scope
		}
		w := httptest.NewRecorder()
		s.answerHere(w, r, live, approveRequest{Approved: approved, RequestID: "ev-a", Scope: scope})
		if w.Code != http.StatusNoContent {
			t.Fatalf("answer = %d %s", w.Code, w.Body)
		}
		if got := <-c; got.ok != approved {
			t.Fatalf("turn took %v, want %v", got.ok, approved)
		}
		if answer.By != agent.ByReviewer || answer.Approver != "olga@example.com" || answer.Granted != scope {
			t.Fatalf("approved %v: answer %+v", approved, answer)
		}
	}
}

// anyLive is the one session the server is running.
func (s *Server) anyLive(t *testing.T) *liveSession {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, l := range s.running {
		return l
	}
	t.Fatal("no session is running")
	return nil
}

// An answer whose scope is not the one offered approves once: the session
// remembers nothing, and the record names no scope granted.
func TestAnAnswerWithAnotherScopeIsNotRemembered(t *testing.T) {
	live := &liveSession{allowed: map[string]bool{}}
	res := policy.Result{Decision: policy.Ask, Step: "default", Scope: "bash(ls *)"}
	go func() {
		p := waitAsked(live, "")
		<-p.ready
		live.mu.Lock()
		live.move(p, askAnswered, true, "bash(*)")
		live.mu.Unlock()
	}()
	ctx, answer := agent.ExpectAnswer(context.Background())
	if ok, err := live.Approve(ctx, "bash", nil, res); err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if len(live.allowed) != 0 || answer.Granted != "" {
		t.Fatalf("allowed %v answer %+v", live.allowed, answer)
	}
}

// A durable row answered with no one signed in says "anonymous"; the record
// then names no approver, as on the in-memory path.
func TestAnAnonymousDurableAnswerNamesNoApprover(t *testing.T) {
	f := newFakeApprovals()
	l := &liveSession{ID: "s-anon", allowed: map[string]bool{}, durable: f}
	go func() {
		for f.askedCount() == 0 {
			time.Sleep(time.Millisecond)
		}
		_, _ = f.AnswerApproval(context.Background(), "ap-test", true, "", "anonymous")
	}()
	ctx, answer := agent.ExpectAnswer(context.Background())
	if ok, err := l.Approve(ctx, "bash", []byte(`{}`), policy.Result{Decision: policy.Ask, Step: "default"}); err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if answer.By != agent.ByReviewer || answer.Approver != "" {
		t.Fatalf("answer %+v", answer)
	}
}

// Through the real sign-in, an answer names the account that gave it; with
// no one signed in it names no one.
func TestAnApprovalOverHTTPNamesTheSignedInApprover(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	if err := local.CreateUser(context.Background(), auth.User{Username: "alice"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	signed := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}),
		Auth:     &auth.Middleware{Providers: []auth.Provider{local}, PublicPaths: local.PublicPaths()}})
	rec := httptest.NewRecorder()
	signed.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"alice","password":"correct-horse-1"}`)))
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "abhed_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("sign-in set no session cookie: %d %s", rec.Code, rec.Body)
	}

	for _, tc := range []struct {
		name   string
		s      *Server
		cookie *http.Cookie
		want   string
	}{
		{"signed in", signed, cookie, "alice"},
		{"no one signed in", testServer(t), nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.s.Handler()
			send := func(method, path, body string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				if tc.cookie != nil {
					r.AddCookie(tc.cookie)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			var created createResponse
			if w := send("POST", "/v1/sessions", `{"workbench":true}`); json.Unmarshal(w.Body.Bytes(), &created) != nil || created.SessionID == "" {
				t.Fatalf("create: %d %s", w.Code, w.Body)
			}
			live := tc.s.anyLive(t)
			ctx, answer := agent.ExpectAnswer(agent.WithRequestID(context.Background(), "ev-a"))
			c := make(chan turnResult, 1)
			go func() {
				ok, err := live.Approve(ctx, "bash", nil, policy.Result{Decision: policy.Ask, Step: "default", Scope: "bash(mkdir *)"})
				c <- turnResult{ok, err}
			}()
			<-waitAsked(live, "ev-a").ready
			if w := send("POST", "/v1/sessions/"+created.SessionID+"/approve", `{"approved":true,"request_id":"ev-a","scope":"bash(mkdir *)"}`); w.Code != http.StatusNoContent {
				t.Fatalf("approve: %d %s", w.Code, w.Body)
			}
			if res := <-c; !res.ok {
				t.Fatal("the turn did not take the approval")
			}
			if answer.By != agent.ByReviewer || answer.Approver != tc.want || answer.Granted != "bash(mkdir *)" {
				t.Fatalf("answer %+v, want approver %q", answer, tc.want)
			}
		})
	}
}
