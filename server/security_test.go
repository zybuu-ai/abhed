package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// These tests exist because the server is reachable from the public internet.
// Each one pins a control that, if it silently regressed, would not break any
// feature — which is exactly why a test has to be the thing that notices.

// A client asking for a weaker permission mode is asking to disable the
// approval gate on an agent that runs shell commands. The request body is
// attacker-controlled, so the mode in it is a request, not an instruction.
func TestClientCannotEscalatePermissionMode(t *testing.T) {
	for _, mode := range []string{"bypass", "auto", "accept-edits"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t)
			body := fmt.Sprintf(`{"prompt":"hi","mode":%q}`, mode)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(
				"POST", "/v1/sessions", strings.NewReader(body)))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("mode %q was accepted with status %d — "+
					"a caller can disable the approval gate", mode, rec.Code)
			}
		})
	}
}

// plan is strictly read-only, so a client narrowing itself to it is safe and
// must keep working: the rule is "never widen", not "never choose".
func TestClientMayNarrowToPlanMode(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(
		"POST", "/v1/sessions", strings.NewReader(`{"prompt":"hi","mode":"plan"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("plan mode rejected with %d; narrowing must stay allowed", rec.Code)
	}
}

// An unknown mode must be refused rather than quietly ignored: falling back to
// the configured mode would let a typo read as success while running with
// permissions the caller did not ask for.
func TestUnknownModeIsRejected(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(
		"POST", "/v1/sessions", strings.NewReader(`{"prompt":"hi","mode":"paln"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unknown mode accepted with %d", rec.Code)
	}
}

// Session IDs used to be a nanosecond timestamp in base36, which is monotonic
// and therefore enumerable: one ID narrows the search for the next.
func TestSessionIDsAreNotGuessable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newSessionID()
		if seen[id] {
			t.Fatalf("duplicate session id %q", id)
		}
		seen[id] = true
	}

	// Two IDs minted back to back must not share a long prefix; a timestamp
	// scheme would differ only in the last character or two.
	a, b := newSessionID(), newSessionID()
	common := 0
	for common < len(a) && common < len(b) && a[common] == b[common] {
		common++
	}
	if common > 6 {
		t.Fatalf("consecutive ids share a %d-char prefix (%q, %q) — "+
			"they look sequential, not random", common, a, b)
	}
}

// The session ID becomes a directory name. filepath.Join CLEANS "..", which
// resolves the traversal rather than refusing it, so the segment must be
// rejected before it is ever joined.
func TestUploadRejectsTraversalInSessionID(t *testing.T) {
	for _, id := range []string{"../escape", "../../etc", "a/b", ".", "..", "a b"} {
		if validSessionID(id) {
			t.Errorf("session id %q accepted as a path segment — "+
				"uploads could be written outside the upload directory", id)
		}
	}
	for _, id := range []string{"s-abc123", "staged-9f8e7d", "A_b-C"} {
		if !validSessionID(id) {
			t.Errorf("legitimate session id %q rejected", id)
		}
	}
}

// A cookie-authenticated console is only safe from cross-site writes if
// something checks where the request came from. SameSite=Lax does it today, but
// it is one flag change away from not doing so, and proxy-header deployments
// have no cookie to attach it to.
func TestCrossOriginWritesAreRejected(t *testing.T) {
	s := testServer(t)
	req := httptest.NewRequest("POST", "/v1/sessions",
		strings.NewReader(`{"prompt":"hi"}`))
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST accepted with %d — CSRF is possible", rec.Code)
	}
}

// A request from the console's own origin is the normal case and must pass.
func TestSameOriginWritesAreAllowed(t *testing.T) {
	s := testServer(t)
	req := httptest.NewRequest("POST", "/v1/sessions",
		strings.NewReader(`{"prompt":"hi"}`))
	req.Header.Set("Origin", "http://"+req.Host)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("same-origin POST rejected with %d", rec.Code)
	}
}

// curl, the SDK and CI send no Origin at all. They are not the threat — CSRF
// needs a browser — so omitting the header must not break them.
func TestRequestsWithoutOriginAreAllowed(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(
		"POST", "/v1/sessions", strings.NewReader(`{"prompt":"hi"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("origin-less POST rejected with %d — API clients would break", rec.Code)
	}
}

// Headers must be present on every response, errors included: a 404 is still a
// response a browser will act on.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{"/", "/console", "/v1/health", "/nope"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))

		for header, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := rec.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", path, header, got, want)
			}
		}
	}
}

// HSTS pins a browser to HTTPS for a year. Sent from a laptop on plain HTTP it
// would pin localhost to a scheme that does not answer, and the pin outlives
// the mistake — so it is only emitted when the operator says TLS is in front.
func TestHSTSOnlyWhenConfigured(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if h := rec.Header().Get("Strict-Transport-Security"); h != "" {
		t.Errorf("HSTS sent without TLS configured: %q", h)
	}

	s.opts.Config.Server.HSTS = true
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if h := rec.Header().Get("Strict-Transport-Security"); h == "" {
		t.Error("HSTS not sent even though TLS is configured")
	}
}

// Sign-in is public by necessity and costs a bcrypt comparison per attempt, so
// unthrottled it is both a credential-stuffing endpoint and a way to burn the
// server's CPU for free.
func TestSignInIsRateLimited(t *testing.T) {
	l := newLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("attempt %d blocked while under the limit", i+1)
		}
	}
	if l.allow("1.2.3.4") {
		t.Fatal("limiter allowed a 4th attempt past a limit of 3")
	}
	// The limit is per-key, so one attacker must not lock out everyone else.
	if !l.allow("5.6.7.8") {
		t.Fatal("a different address was blocked by another's attempts")
	}
}

// X-Forwarded-For is attacker-controlled unless a proxy we trust set it. If it
// were read by default, a client could rotate its own limiter key and never be
// throttled at all.
func TestForwardedForIgnoredUnlessProxyTrusted(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/signin", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "1.1.1.1")

	if got := clientIP(r, false); got != "10.0.0.1" {
		t.Errorf("untrusted XFF was honoured: got %q, want the socket address", got)
	}
	if got := clientIP(r, true); got != "1.1.1.1" {
		t.Errorf("trusted XFF ignored: got %q", got)
	}
}

// A body with no cap can be streamed indefinitely into a decode buffer by an
// unauthenticated caller.
func TestOversizedBodyIsRejected(t *testing.T) {
	s := testServer(t)
	huge := `{"prompt":"` + strings.Repeat("A", (2<<20)) + `"}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(
		"POST", "/v1/sessions", strings.NewReader(huge)))

	if rec.Code == http.StatusAccepted {
		t.Fatal("a 2 MiB body was accepted; the request cap is not applied")
	}
}

// A handler panic must not take the connection down silently: without recovery
// the request vanishes from the log, which is the worst outcome for something
// internet-facing.
func TestPanicIsRecoveredAndLogged(t *testing.T) {
	s := testServer(t)
	h := s.withMiddleware(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { panic("boom") }))

	rec := httptest.NewRecorder()
	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("panic escaped the middleware: %v", v)
		}
	}()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("panic produced status %d, want 500", rec.Code)
	}
	// A Go stack trace names packages, paths and versions.
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("panic detail leaked to the client")
	}
}

// The overview is public so the landing page can describe the deployment before
// sign-in, but the workspace is an absolute host path: it names the operator's
// account and directory layout to anyone who asks.
func TestAnonymousOverviewHidesWorkspacePath(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/overview", nil))

	var o overviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	if o.Workspace != "" {
		t.Errorf("workspace path disclosed anonymously: %q", o.Workspace)
	}
	// The rest must still be there, or the landing page cannot stay honest.
	if o.Model == "" {
		t.Error("overview no longer names the model")
	}
}

// The session LIST leaked across users while the single-session fetch did not.
// That asymmetry is how the bug survived: fetching another user's transcript
// 404'd correctly, so the ownership check looked present, while the list handed
// out everyone's prompts — which name what people are working on and what they
// uploaded.
//
// Found in production by a user seeing another user's chat history.
func TestSessionListIsPerUser(t *testing.T) {
	s := proxyServer(t)

	mk := func(user, prompt string) {
		req := httptest.NewRequest("POST", "/v1/sessions",
			strings.NewReader(`{"prompt":"`+prompt+`"}`))
		req.Header.Set("X-Abhed-User", user)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("create for %s: %d", user, rec.Code)
		}
	}
	mk("alice", "alice private prompt")
	mk("bob", "bob private prompt")
	time.Sleep(200 * time.Millisecond)

	list := func(user string) string {
		req := httptest.NewRequest("GET", "/v1/sessions", nil)
		req.Header.Set("X-Abhed-User", user)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Body.String()
	}

	if got := list("bob"); strings.Contains(got, "alice private prompt") {
		t.Fatalf("bob's session list contains alice's prompt — history leak:\n%s", got)
	}
	if got := list("alice"); !strings.Contains(got, "alice private prompt") {
		t.Fatalf("alice cannot see her own session:\n%s", got)
	}
}

// A proxy user named like a reserved owner ("anonymous", "agent") is an
// ordinary user, not the no-auth caller who sees the whole tenant.
func TestProxyUserNamedLikeAReservedOwner(t *testing.T) {
	s := proxyServer(t)
	send := func(method, path, user, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Abhed-User", user)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	rec := send("POST", "/v1/sessions", "alice", `{"prompt":"alice private prompt"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d", rec.Code)
	}
	var created struct {
		ID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("create: %v %s", err, rec.Body)
	}
	time.Sleep(200 * time.Millisecond)
	for _, user := range []string{"anonymous", "ANONYMOUS", "Anonymous", " anonymous ", "\tanonymous", "agent", "AGENT", " agent "} {
		if got := send("GET", "/v1/sessions", user, "").Body.String(); strings.Contains(got, "alice private prompt") {
			t.Errorf("proxy user %q lists alice's session:\n%s", user, got)
		}
		if code := send("GET", "/v1/sessions/"+created.ID, user, "").Code; code != http.StatusNotFound {
			t.Errorf("proxy user %q opens alice's session: %d", user, code)
		}
		if code := send("POST", "/v1/sessions/"+created.ID+"/approve", user, `{"approved":false}`).Code; code != http.StatusNotFound {
			t.Errorf("proxy user %q answers alice's approval: %d", user, code)
		}
		if code := send("POST", "/v1/sessions/"+created.ID+"/messages", user, `{"prompt":"steer"}`).Code; code != http.StatusNotFound {
			t.Errorf("proxy user %q steers alice's session: %d", user, code)
		}
	}
	send("POST", "/v1/sessions", "anonymous", `{"prompt":"anon own prompt"}`)
	time.Sleep(200 * time.Millisecond)
	if got := send("GET", "/v1/sessions", "anonymous", "").Body.String(); !strings.Contains(got, "anon own prompt") {
		t.Errorf("proxy user anonymous cannot see its own session:\n%s", got)
	}
}

// A proxy request that names no one who can own (an email that is not an
// address) is refused before it can create a session it could never open.
func TestNobodyCallerIsRefused(t *testing.T) {
	s := proxyServer(t)
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/v1/sessions", `{"prompt":"orphan"}`},
		{"GET", "/v1/sessions", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("X-Abhed-Email", "not-an-address")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
	s.mu.RLock()
	n := len(s.running)
	s.mu.RUnlock()
	if n != 0 {
		t.Errorf("%d session(s) created for a caller who names no one", n)
	}
}

// A removed account's running sessions go to no one, so an account made
// again under the same name does not inherit them.
func TestReleaseSessionsUnclaimsLiveSessions(t *testing.T) {
	s := proxyServer(t)
	s.mu.Lock()
	s.running["a"] = &liveSession{ID: "a", Tenant: "default", User: "local:bob"}
	s.running["b"] = &liveSession{ID: "b", Tenant: "other", User: "local:bob"}
	s.running["c"] = &liveSession{ID: "c", Tenant: "default", User: "local:carol"}
	s.mu.Unlock()
	if n := s.ReleaseSessions("default", "local:bob"); n != 1 {
		t.Fatalf("released %d, want 1", n)
	}
	if _, ok := s.session("a", "default", "local:bob"); ok {
		t.Error("a new bob still owns the old bob's session")
	}
	if s.running["a"].User != "unclaimed:local:bob" || s.running["b"].User != "local:bob" || s.running["c"].User != "local:carol" {
		t.Errorf("owners after release: %q %q %q", s.running["a"].User, s.running["b"].User, s.running["c"].User)
	}
	if n := s.ReleaseSessions("default", "anonymous"); n != 0 {
		t.Errorf("released %d for the no-auth owner", n)
	}
	s.mu.Lock()
	delete(s.running, "a")
	delete(s.running, "b")
	delete(s.running, "c")
	s.mu.Unlock()
}

// The list and the single-session fetch must agree. A list more permissive than
// the fetch leaks; one that is stricter hides sessions the user can open.
func TestListAndFetchAgreeOnOwnership(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		recTenant, recUser, tenant, user string
		want                             bool
	}{
		{"same user", "t1", "alice", "t1", "alice", true},
		{"different user", "t1", "alice", "t1", "bob", false},
		{"different tenant", "t1", "alice", "t2", "alice", false},
		{"anonymous sees all in tenant", "t1", "alice", "t1", "anonymous", true},
		{"anonymous is still tenant-scoped", "t1", "alice", "t2", "anonymous", false},
	} {
		if got := ownsSession(tc.recTenant, tc.recUser, tc.tenant, tc.user); got != tc.want {
			t.Errorf("%s: ownsSession = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A file the agent produced was reachable only as a path like
// /workspace/report.docx — a location inside a container the reader cannot
// open. The work was done and then stranded.
func TestDownloadRequiresOwnership(t *testing.T) {
	s := proxyServer(t)
	if err := os.WriteFile(filepath.Join(s.opts.Workspace, "report.docx"),
		[]byte("PK\x03\x04fake"), 0o600); err != nil {
		t.Fatal(err)
	}

	mk := func(user string) string {
		req := httptest.NewRequest("POST", "/v1/sessions",
			strings.NewReader(`{"prompt":"hi"}`))
		req.Header.Set("X-Abhed-User", user)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		var out struct {
			SessionID string `json:"session_id"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out.SessionID
	}
	sid := mk("alice")
	time.Sleep(150 * time.Millisecond)

	get := func(user, id, path string) int {
		req := httptest.NewRequest("GET",
			"/v1/sessions/"+id+"/download?path="+path, nil)
		req.Header.Set("X-Abhed-User", user)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	if code := get("alice", sid, "report.docx"); code != http.StatusOK {
		t.Errorf("owner could not download their own file: %d", code)
	}
	if code := get("bob", sid, "report.docx"); code == http.StatusOK {
		t.Error("another user downloaded a file from someone else's session")
	}
}

// A path is attacker-controlled input. filepath.Join CLEANS "..", which
// resolves a traversal rather than refusing it, so the workspace boundary has
// to be proven after resolution rather than assumed before it.
func TestDownloadRejectsEscape(t *testing.T) {
	s := proxyServer(t)
	for _, p := range []string{
		"../../etc/passwd", "/etc/passwd", "..%2F..%2Fetc%2Fpasswd",
	} {
		if _, err := s.resolveInWorkspace(p); err == nil {
			t.Errorf("path %q escaped the workspace", p)
		}
	}
}

// A transcript can hold a pasted credential or an uploaded document. "You
// cannot remove that" is the wrong answer, and a delete that silently does
// nothing is worse than no delete at all.
func TestDeleteSessionRemovesTranscript(t *testing.T) {
	s := proxyServer(t)
	req := httptest.NewRequest("POST", "/v1/sessions",
		strings.NewReader(`{"prompt":"secret"}`))
	req.Header.Set("X-Abhed-User", "alice")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out struct {
		SessionID string `json:"session_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	time.Sleep(200 * time.Millisecond)

	del := func(user string) int {
		r := httptest.NewRequest("DELETE", "/v1/sessions/"+out.SessionID, nil)
		r.Header.Set("X-Abhed-User", user)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Code
	}

	if code := del("bob"); code == http.StatusNoContent {
		t.Fatal("another user deleted someone else's session")
	}
	if code := del("alice"); code != http.StatusNoContent {
		t.Fatalf("owner could not delete their own session: %d", code)
	}

	r := httptest.NewRequest("GET", "/v1/sessions/"+out.SessionID+"/replay", nil)
	r.Header.Set("X-Abhed-User", "alice")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("transcript still readable after delete: %d", w.Code)
	}
}

// Every local account was equal until the admin group existed. That was
// survivable while a user could only run their own sessions, and stops being
// survivable the moment the console can add a skill or an MCP server — a skill
// is instructions, so granting one is granting the power to rewrite what the
// agent does.
//
// Both directions are tested deliberately: a gate that never denies is not a
// gate, and a gate that never admits is an outage.
func TestAdminRoutesRequireTheAdminGroup(t *testing.T) {
	s := proxyServer(t)

	call := func(user, groups, method, path string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.Header.Set("X-Abhed-User", user)
		if groups != "" {
			req.Header.Set("X-Abhed-Groups", groups)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	// The built-in administrative surface. Routes an edition mounts are
	// checked the same way where they are mounted, through the same Admin
	// wrapper, so there is one definition of administrator to get wrong.
	routes := []struct{ method, path string }{
		{"GET", "/v1/admin/users"},
		{"POST", "/v1/admin/users/admin"},
		{"GET", "/v1/admin/settings"},
	}
	for _, rt := range routes {
		// Forbidden, specifically. A route that answers 501 to a non-admin
		// because its backing store is absent has not been authorised — it has
		// merely failed earlier, and would let the request through the moment
		// the store exists. This caught exactly that ordering bug.
		if code := call("bob", "", rt.method, rt.path); code != http.StatusForbidden {
			t.Errorf("%s %s: non-admin got %d, want 403 — admin surface is open",
				rt.method, rt.path, code)
		}
		if code := call("alice", DefaultAdminGroup, rt.method, rt.path); code == http.StatusForbidden {
			t.Errorf("%s %s: admin was refused — the gate never admits",
				rt.method, rt.path)
		}
	}

	// A group that merely looks adjacent must not pass.
	if code := call("eve", "abhed-admins,admin", "GET", "/v1/admin/users"); code != http.StatusForbidden {
		t.Errorf("a near-miss group name was accepted: %d", code)
	}
}

// Signup is closed by default because Abhed runs shell commands. With no
// invite mechanism mounted, closed means closed, and the refusal says so
// rather than asking for a code that nothing could have issued.
func TestSignupIsClosedWithoutInvites(t *testing.T) {
	// Local accounts, because signup only exists where Abhed holds them.
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	cfg.Auth.AllowSignup = false
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	s := New(Options{
		Workspace: t.TempDir(),
		Config:    cfg,
		Adapter:   stubAdapter{},
		Registry:  tools.NewRegistry(tools.Read{}),
		Auth: &auth.Middleware{Providers: []auth.Provider{local},
			PublicPaths: []string{"/v1/signup"}},
	})

	body := `{"username":"mallory","password":"correct-horse-battery"}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(
		"POST", "/v1/signup", strings.NewReader(body)))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("closed registration answered %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "disabled") {
		t.Errorf("the refusal does not say registration is closed: %s", rec.Body.String())
	}
}

// A client names a provider from the configured set. It must never be able to
// supply a URL or a key: that would let a session point the agent at a host of
// the caller's choosing and deliver every prompt and every file the agent had
// read straight to it.
func TestSessionProviderMustBeConfigured(t *testing.T) {
	s := proxyServer(t)

	for _, name := range []string{
		"http://evil.example/v1",
		"https://attacker.test",
		"not-configured",
		"../local",
	} {
		body := `{"prompt":"hi","provider":"` + name + `"}`
		req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(body))
		req.Header.Set("X-Abhed-User", "alice")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code == http.StatusAccepted {
			t.Errorf("provider %q was accepted — a caller can choose the endpoint", name)
		}
	}
}

// The picker only offers what the deployment configured, and the list has to be
// stable or the dropdown reshuffles under the user on every poll.
func TestProviderListIsStableAndScoped(t *testing.T) {
	s := proxyServer(t)
	s.opts.Config.Model.Default = "b"
	s.opts.Config.Model.Providers = map[string]config.ProviderConfig{
		"c": {Type: "ollama", Model: "m3"},
		"a": {Type: "ollama", Model: "m1"},
		"b": {Type: "ollama", Model: "m2"},
	}

	first := s.providers()
	if len(first) != 3 {
		t.Fatalf("expected 3 providers, got %d", len(first))
	}
	if first[0].Name != "a" || first[2].Name != "c" {
		t.Errorf("provider list is not sorted: %v", first)
	}
	if !first[1].Default {
		t.Error("the configured default is not marked")
	}
	// A key must never reach the client.
	raw, _ := json.Marshal(first)
	if strings.Contains(string(raw), "api_key") {
		t.Error("the provider list leaks credential fields")
	}
}

// The tool registry is read on every turn of every session. A settings change
// that mutated it in place would race with those readers, so a change clones,
// mutates the clone, and swaps the pointer.
//
// Run with -race: without the copy-on-write this fails, and it fails as a
// corrupted map rather than a clean error.
func TestToolRegistrySwapIsRaceFree(t *testing.T) {
	st := &mutable{registry: tools.NewRegistry(tools.Read{}, tools.Glob{})}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers, standing in for live sessions.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					reg := st.toolRegistry()
					_ = reg.Names()
					_ = reg.Definitions()
					_, _ = reg.Get("read")
				}
			}
		}()
	}

	// Writers, standing in for settings changes.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				st.swapTools(func(reg *tools.Registry) { reg.Add(tools.Grep{}) })
			}
		}()
	}

	time.Sleep(120 * time.Millisecond)
	close(stop)
	wg.Wait()

	// The tool added by the writers must actually be there afterwards.
	if _, ok := st.toolRegistry().Get("grep"); !ok {
		t.Error("a swapped-in tool is missing after the swaps settled")
	}
	// And the originals must survive: a clone that dropped entries would be a
	// silent capability loss.
	if _, ok := st.toolRegistry().Get("read"); !ok {
		t.Error("clone lost a pre-existing tool")
	}
}

// A registry clone must be independent: mutating the copy must not reach into
// the original a running session is still holding.
func TestRegistryCloneIsIndependent(t *testing.T) {
	orig := tools.NewRegistry(tools.Read{})
	clone := orig.Clone()
	clone.Add(tools.Grep{})

	if _, ok := orig.Get("grep"); ok {
		t.Error("mutating a clone changed the original")
	}
	if _, ok := clone.Get("read"); !ok {
		t.Error("the clone lost the original's tools")
	}

	clone.Remove("read")
	if _, ok := orig.Get("read"); !ok {
		t.Error("removing from a clone removed from the original")
	}
}

// Images are identified by magic bytes, never by the filename. The name comes
// from the uploader, so trusting ".png" would let anything at all be presented
// to the model as an image.
func TestImageDetectionUsesMagicBytes(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"png", []byte("\x89PNG\r\n\x1a\n....."), "image/png"},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0}, "image/jpeg"},
		{"gif", []byte("GIF89a......"), "image/gif"},
		{"webp", []byte("RIFF....WEBPVP8 "), "image/webp"},
		{"plain text", []byte("hello, this is text"), ""},
		{"empty", []byte{}, ""},
		{"a lie", []byte("this is not a png at all"), ""},
	}
	for _, tc := range cases {
		if got := imageMediaType(tc.data); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCanonicalHostRedirect pins the rename contract: the old hostname keeps
// answering, but only by sending the visitor to the new one, and never by
// serving the console under both names.
func TestCanonicalHostRedirect(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := canonicalHost("abhed.example.com", inner)

	req := httptest.NewRequest(http.MethodGet, "/console?x=1", nil)
	req.Host = "old.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("old host: got %d, want 301", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "https://abhed.example.com/console?x=1" {
		t.Fatalf("old host: Location = %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/console", nil)
	req.Host = "ABHED.example.com"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("canonical host (any case): got %d, want the handler", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/sessions", nil)
	req.Host = "old.example.com"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("POST to old host: got %d, want 421", rec.Code)
	}

	// Unset means no redirect at all: a laptop answers on whatever it is called.
	rec = httptest.NewRecorder()
	canonicalHost("", inner).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("no canonical host: got %d, want the handler", rec.Code)
	}
}

// The picker lists what the configuration names: the built-in local provider,
// merged in from the defaults, is left out unless it is the default.
func TestProviderListLeavesOutAnUnconfiguredBuiltIn(t *testing.T) {
	s := proxyServer(t)
	s.opts.Config.Model.Default = "a"
	s.opts.Config.Model.Providers = config.Default().Model.Providers
	s.opts.Config.Model.Providers["a"] = config.ProviderConfig{Type: "ollama", Model: "m1"}
	s.opts.Config.Model.Providers["b"] = config.ProviderConfig{Type: "ollama", Model: "m2"}
	s.opts.Config.SetKeys = []string{"model.default", "model.providers.a", "model.providers.b"}
	var names []string
	for _, p := range s.providers() {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "a,b" {
		t.Fatalf("the picker lists %v, want [a b]", names)
	}
	s.opts.Config.Model.Default = "local"
	if len(s.providers()) != 3 {
		t.Fatal("the built-in provider is not listed when it is the default")
	}
}
