package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/profiles"
)

type acpAuthMethod struct {
	ID string `json:"id"`
}

func initAuth(t *testing.T, cl *studioClient) ([]string, map[string]any) {
	t.Helper()
	var res struct {
		AuthMethods       []acpAuthMethod `json:"authMethods"`
		AgentCapabilities struct {
			Meta map[string]map[string]any `json:"_meta"`
		} `json:"agentCapabilities"`
	}
	cl.ok("initialize", map[string]any{"protocolVersion": 1}, &res)
	var ids []string
	for _, m := range res.AuthMethods {
		ids = append(ids, m.ID)
	}
	id, _ := res.AgentCapabilities.Meta[acpMetaKey]["identity"].(map[string]any)
	return ids, id
}

// The engine lists the ways in; with no one signed in it works as guest by
// default and says the choice was not made, so Studio shows it; signing in
// before a session opens moves the record to the account.
func TestACPAuthenticateBeforeASession(t *testing.T) {
	signInTest(t)
	acpModelsEnv(t, `{}`)
	s, _ := identity.Open()
	if _, _, err := s.Create("a1", "correct horse", "", ""); err != nil {
		t.Fatal(err)
	}
	if code := enterPrincipalForClient(signInFlags{}, &strings.Builder{}); code != 0 {
		t.Fatal(code)
	}
	cl := newStudioClient(t, t.TempDir(), true)
	ids, who := initAuth(t, cl)
	if strings.Join(ids, ",") != "abhed.account,abhed.register,abhed.server,abhed.guest" {
		t.Fatalf("auth methods %v", ids)
	}
	if who["kind"] != "guest" || who["chosen"] != false {
		t.Fatalf("identity %v", who)
	}
	msg := cl.refusedMsg("authenticate", map[string]any{"methodId": "abhed.account",
		"_meta": map[string]any{acpMetaKey: map[string]any{"username": "a1", "password": "nope nope nope"}}})
	if !strings.Contains(msg, "incorrect username or password") {
		t.Fatalf("wrong password: %s", msg)
	}
	var res map[string]any
	cl.ok("authenticate", map[string]any{"methodId": "abhed.account",
		"_meta": map[string]any{acpMetaKey: map[string]any{"username": "a1", "password": "correct horse"}}}, &res)
	raw, _ := json.Marshal(res)
	if !strings.Contains(string(raw), `"kind":"account"`) || !strings.Contains(string(raw), `"chosen":true`) {
		t.Fatalf("after authenticate: %s", raw)
	}
	e, _ := s.Get("a1")
	if !strings.Contains(cl.conn.recDir, e.ID) {
		t.Errorf("the record did not follow the account: %s", cl.conn.recDir)
	}
}

// Where sign-in is required, everything but initialize and authenticate is
// refused with -32000 and authRequired until the client signs in; guest is
// not offered when it is off.
func TestACPRequiredSignInRefusesUntilAuthenticated(t *testing.T) {
	signInTest(t)
	acpModelsEnv(t, `{}`)
	withManaged(t, `{"auth":{"client":{"sign_in":"required","guest":false}}}`)
	if code := enterPrincipalForClient(signInFlags{}, &strings.Builder{}); code != 0 {
		t.Fatal(code)
	}
	ws := t.TempDir()
	cl := newStudioClient(t, ws, true)
	ids, who := initAuth(t, cl)
	if strings.Contains(strings.Join(ids, ","), "abhed.guest") || who["pending"] == nil {
		t.Fatalf("methods %v identity %v", ids, who)
	}
	m := cl.call("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})
	if m.Error == nil || m.Error.Code != errRefused || !strings.Contains(m.Error.Message, "Authentication required") {
		t.Fatalf("session/new before sign-in: %+v", m.Error)
	}
	if msg := cl.refusedMsg("authenticate", map[string]any{"methodId": "abhed.guest"}); !strings.Contains(msg, "guest off") {
		t.Fatalf("guest: %s", msg)
	}
	cl.ok("authenticate", map[string]any{"methodId": "abhed.register",
		"_meta": map[string]any{acpMetaKey: map[string]any{"username": "b2", "password": "another horse"}}}, nil)
	var res struct {
		SessionID string `json:"sessionId"`
	}
	cl.ok("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}}, &res)
	if res.SessionID == "" {
		t.Fatal("no session after sign-in")
	}
	if msg := cl.refusedMsg("authenticate", map[string]any{"methodId": "abhed.guest"}); !strings.Contains(msg, "fixed once a session has opened") {
		t.Fatalf("a switch after a session opened: %s", msg)
	}
}

// refusedMsg calls method and returns its error's message, failing when it
// succeeded.
func (cl *studioClient) refusedMsg(method string, params any) string {
	cl.t.Helper()
	m := cl.call(method, params)
	if m.Error == nil {
		cl.t.Fatalf("%s succeeded", method)
	}
	return m.Error.Message
}

// The identity names whose history it is, and _abhed/auth/signout makes the
// next start ask again.
func TestACPIdentityOwnerAndSignOut(t *testing.T) {
	signInTest(t)
	acpModelsEnv(t, `{}`)
	s, _ := identity.Open()
	e, _, err := s.Create("a1", "correct horse", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remember(e.ID); err != nil {
		t.Fatal(err)
	}
	if code := enterPrincipalForClient(signInFlags{}, &strings.Builder{}); code != 0 {
		t.Fatal(code)
	}
	cl := newStudioClient(t, t.TempDir(), true)
	_, who := initAuth(t, cl)
	if who["owner"] != "device:a1" || who["chosen"] != true || who["adopted"] != nil {
		t.Fatalf("identity %v", who)
	}
	cl.ok("_abhed/auth/signout", map[string]any{}, nil)
	if _, ok, _ := s.Remembered(nil); ok {
		t.Error("still remembered after signout")
	}
}

// authenticate and session/new sent together: either the switch is refused,
// or the session opens as the person signed in, never as the one before.
func TestACPAuthenticateRacesSessionNew(t *testing.T) {
	for i := 0; i < 5; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			signInTest(t)
			old := identity.BcryptCost
			identity.BcryptCost = 8 // slow enough to leave a window
			t.Cleanup(func() { identity.BcryptCost = old })
			acpModelsEnv(t, `{}`)
			s, _ := identity.Open()
			if _, _, err := s.Create("a1", "correct horse", "", ""); err != nil {
				t.Fatal(err)
			}
			if code := enterPrincipalForClient(signInFlags{}, &strings.Builder{}); code != 0 {
				t.Fatal(code)
			}
			ws := t.TempDir()
			cl := newStudioClient(t, ws, true)
			initAuth(t, cl)
			var auth, sess rpcMessage
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				auth = cl.call("authenticate", map[string]any{"methodId": "abhed.account",
					"_meta": map[string]any{acpMetaKey: map[string]any{"username": "a1", "password": "correct horse"}}})
			}()
			go func() {
				defer wg.Done()
				sess = cl.call("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})
			}()
			wg.Wait()
			if sess.Error != nil {
				t.Fatalf("session/new: %+v", sess.Error)
			}
			cl.conn.identMu.Lock()
			recUser := cl.conn.recUser
			cl.conn.identMu.Unlock()
			switch {
			case auth.Error == nil:
				if recUser != "device:a1" || currentPrincipal().Kind != identity.Account {
					t.Fatalf("signed in, but the session works as %q (%v)", recUser, currentPrincipal().Kind)
				}
			case strings.Contains(auth.Error.Message, "fixed once a session has opened"):
				if currentPrincipal().Kind != identity.Guest {
					t.Fatalf("refused, but the principal moved to %v", currentPrincipal().Kind)
				}
			default:
				t.Fatalf("authenticate: %+v", auth.Error)
			}
		})
	}
}

// Only an https sign-in page, or an http one on this machine or at the
// server itself, is handed to the client to open.
func TestSignInPageAllowed(t *testing.T) {
	for _, ok := range []struct{ page, base string }{
		{"https://idp.example/authorize?x=1", "https://abhed.corp"},
		{"http://127.0.0.1:5556/dex/auth", "https://abhed.corp"},
		{"http://abhed.lan:8080/cli/login", "http://abhed.lan:8080"},
	} {
		if err := signInPageAllowed(ok.page, ok.base); err != nil {
			t.Errorf("%s: %v", ok.page, err)
		}
	}
	for _, bad := range []struct{ page, base string }{
		{"vscode://abhed.corp/open?x", "https://abhed.corp"},
		{"x-handler://abhed.corp/", "https://abhed.corp"},
		{"javascript:alert(1)", "https://abhed.corp"},
		{"file:///etc/passwd", "https://abhed.corp"},
		{"http://evil.example/", "https://abhed.corp"},
		{"https://u:p@abhed.corp/", "https://abhed.corp"},
	} {
		if err := signInPageAllowed(bad.page, bad.base); err == nil {
			t.Errorf("%s was allowed", bad.page)
		}
	}
}

// While a server sign-in waits for its browser, the engine stays responsive:
// other requests are refused at once with data.signingIn, and
// _abhed/auth/cancel ends the sign-in with nothing kept.
func TestACPServerSignInIsCancellable(t *testing.T) {
	signInTest(t)
	acpModelsEnv(t, `{}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"auth_mode":"local","methods":[{"name":"local","authorization_endpoint":"/cli/login","token_endpoint":"/v1/cli/token"}]}`))
	}))
	defer srv.Close()
	if code := enterPrincipalForClient(signInFlags{}, &strings.Builder{}); code != 0 {
		t.Fatal(code)
	}
	ws := t.TempDir()
	cl := newStudioClient(t, ws, true)
	initAuth(t, cl)
	from := cl.mark()
	done := make(chan rpcMessage, 1)
	go func() {
		done <- cl.call("authenticate", map[string]any{"methodId": "abhed.server",
			"_meta": map[string]any{acpMetaKey: map[string]any{"url": srv.URL}}})
	}()
	cl.waitFor(from, "the sign-in page", func(m rpcMessage) bool { return m.Method == "_abhed/auth/openUrl" })
	start := time.Now()
	m := cl.call("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}})
	if m.Error == nil || !strings.Contains(fmt.Sprint(m.Error.Data), "signingIn") || time.Since(start) > 5*time.Second {
		t.Fatalf("session/new during the sign-in: %+v after %s", m.Error, time.Since(start))
	}
	cl.ok("_abhed/auth/cancel", map[string]any{}, nil)
	select {
	case a := <-done:
		if a.Error == nil || !strings.Contains(a.Error.Message, "cancelled") {
			t.Fatalf("authenticate after cancel: %+v", a)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled sign-in did not end")
	}
	if p := currentPrincipal(); p.Kind != identity.Guest {
		t.Fatalf("principal after a cancelled sign-in: %v", p.Kind)
	}
	s, _ := identity.Open()
	if list, _ := s.List(); len(list) != 0 {
		t.Fatalf("a cancelled sign-in left %d entries", len(list))
	}
	if m := cl.call("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}}); m.Error != nil {
		t.Fatalf("session/new after the cancel: %+v", m.Error)
	}
}

// Sign-out over ACP ends this engine's own sign-in, everywhere: the
// account's revocation count rises, a server's token is revoked there, a
// sign-in another process made since is left alone, and the engine works
// for no one after it.
func TestACPSignOutEndsOnlyThisEnginesSignIn(t *testing.T) {
	signInTest(t)
	acpModelsEnv(t, `{}`)
	revoked := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/cli/whoami":
			_, _ = w.Write([]byte(`{"sub":"pat"}`))
		case "/v1/cli/logout":
			revoked <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	s, _ := identity.Open()
	e, _ := s.AddServer(srv.URL, "pat", "", "org")
	root, _ := s.Root(e)
	prof := &profiles.Profile{URL: srv.URL, Token: "abhcli_x", RevocationEndpoint: srv.URL + "/v1/cli/logout"}
	_ = profiles.Save(filepath.Join(root, "profiles.json"), &profiles.File{Profiles: map[string]*profiles.Profile{"org": prof}, Current: "org"})
	_ = s.Remember(e.ID)
	if code := enterPrincipalForClient(signInFlags{}, &strings.Builder{}); code != 0 {
		t.Fatal(code)
	}
	if currentPrincipal().Kind != identity.Server {
		t.Fatalf("principal %v", currentPrincipal().Kind)
	}
	before, _ := s.ByID(e.ID)
	cl := newStudioClient(t, t.TempDir(), true)
	initAuth(t, cl)

	// Another process signs someone else in meanwhile.
	other, _, _ := s.Create("b2", "correct horse", "", "")
	_ = s.Remember(other.ID)

	cl.ok("_abhed/auth/signout", map[string]any{}, nil)
	select {
	case <-revoked:
	default:
		t.Error("the server token was not revoked")
	}
	if rem, ok, _ := s.Remembered(nil); !ok || rem.ID != other.ID {
		t.Error("sign-out ended another process's sign-in")
	}
	if after, _ := s.ByID(e.ID); after.Revocations <= before.Revocations {
		t.Error("the revocation count did not rise")
	}
	reg, _ := profiles.Load(filepath.Join(root, "profiles.json"))
	if _, p, _ := reg.Get("org"); p != nil && p.Token != "" {
		t.Error("the server token is still kept")
	}
	m := cl.call("session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
	if m.Error == nil || !strings.Contains(m.Error.Message, "signed out") {
		t.Fatalf("session/new after sign-out: %+v", m.Error)
	}
}
