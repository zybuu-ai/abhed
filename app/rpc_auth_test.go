package app

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/home"
	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/profiles"
)

// signInTest runs a test as a real start would: no test-only fallback, and
// no principal left over for the next test.
func signInTest(t *testing.T) {
	t.Helper()
	accountHome(t)
	old := unresolvedIsLegacy
	unresolvedIsLegacy = false
	t.Cleanup(func() {
		unresolvedIsLegacy = old
		endGuest()
		setPendingSignIn(nil)
	})
}

// A client signs the person in over rpc before it starts a session; once
// one has started, who is working is fixed.
func TestRPCAuthSignsInBeforeStart(t *testing.T) {
	signInTest(t)
	s, _ := identity.Open()
	if _, _, err := s.Create("a1", "correct horse", "", ""); err != nil {
		t.Fatal(err)
	}
	if code := enterPrincipalForClient(signInFlags{}, &strings.Builder{}); code != 0 {
		t.Fatalf("gate: %d", code)
	}
	got := runRPC(t, t.TempDir(),
		`{"id":"1","method":"auth.status"}`,
		`{"id":"2","method":"auth.signin","username":"a1","password":"wrong one!"}`,
		`{"id":"3","method":"auth.signin","username":"a1","password":"correct horse"}`,
		`{"id":"4","method":"quit"}`)
	for _, want := range []string{`"id":"1","type":"auth","principal":{"kind":"guest"`,
		`"id":"2","type":"error","error":"incorrect username or password"`,
		`"id":"3","type":"auth","principal":{"kind":"account","display":"a1"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "correct horse") {
		t.Error("the password came back in a reply")
	}
	if p := currentPrincipal(); p.Kind != identity.Account {
		t.Errorf("principal after sign-in: %+v", p)
	}
}

// Under a required sign-in, rpc does not exit: start is refused until the
// client signs the person in, and guest is refused when it is turned off.
func TestRPCRequiredSignInWaitsForTheClient(t *testing.T) {
	signInTest(t)
	setPendingSignIn(&identity.RequiredError{Why: "your organisation requires it"})
	got := runRPC(t, t.TempDir(),
		`{"id":"1","method":"start"}`,
		`{"id":"2","method":"auth.status"}`,
		`{"id":"3","method":"auth.register","username":"b2","password":"another horse"}`,
		`{"id":"4","method":"quit"}`)
	if !strings.Contains(got, `"id":"1","type":"error","error":"sign-in required: your organisation requires it; send auth.signin`) {
		t.Errorf("start was not refused:\n%s", got)
	}
	if !strings.Contains(got, `"id":"2","type":"auth","principal":{"kind":"","display":"","pending":"sign-in required`) {
		t.Errorf("status did not say a sign-in is pending:\n%s", got)
	}
	if !strings.Contains(got, `"id":"3","type":"auth","principal":{"kind":"account","display":"b2"},"recovery_code":"`) {
		t.Errorf("register:\n%s", got)
	}
	if err := pendingSignIn(); err != nil {
		t.Errorf("still pending after register: %v", err)
	}
}

func TestSwitchPrincipalRefusesGuestWhenOff(t *testing.T) {
	signInTest(t)
	withManaged(t, `{"auth":{"client":{"guest":false}}}`)
	if _, err := switchPrincipal("auth.guest", "", "", ""); !errors.Is(err, identity.ErrSignInRequired) {
		t.Fatalf("guest with guest off: %v", err)
	}
}

// withManaged makes body the managed configuration for the test.
func withManaged(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
}

// A remembered server sign-in whose server says the organisation removed
// the person's access ends at start: the run is refused, the sign-in is
// forgotten, and the person's data on this machine stays.
func TestRemovedAccessEndsTheRememberedServerSignIn(t *testing.T) {
	signInTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"access_revoked","access_removed":true,"reason":"removed by the organisation"}`))
	}))
	defer srv.Close()
	s, _ := identity.Open()
	e, err := s.AddServer(srv.URL, "pat", "pat@example.com", "org")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := s.Root(e)
	reg := &profiles.File{Profiles: map[string]*profiles.Profile{"org": {URL: srv.URL, Username: "pat", Token: "abhcli_x"}}, Current: "org"}
	if err := profiles.Save(filepath.Join(root, "profiles.json"), reg); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(root, "history", "h.jsonl")
	_ = os.MkdirAll(filepath.Dir(keep), 0o700)
	_ = os.WriteFile(keep, []byte("{}\n"), 0o600)
	if err := s.Remember(e.ID); err != nil {
		t.Fatal(err)
	}
	_, err = resolvePrincipal(signInFlags{}, false, os.Stdin, io.Discard)
	if !errors.Is(err, identity.ErrSignInRequired) || !strings.Contains(err.Error(), "removed your access") {
		t.Fatalf("err = %v", err)
	}
	if _, ok, _ := s.Remembered(nil); ok {
		t.Error("the removed sign-in is still remembered")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("the person's data went with the access")
	}
}

// A server that cannot be reached does not end a remembered sign-in.
func TestUnreachableServerKeepsTheSignIn(t *testing.T) {
	signInTest(t)
	old := serverCheckWait
	serverCheckWait = 200 * time.Millisecond
	t.Cleanup(func() { serverCheckWait = old })
	s, _ := identity.Open()
	e, _ := s.AddServer("http://127.0.0.1:1", "pat", "", "org")
	root, _ := s.Root(e)
	reg := &profiles.File{Profiles: map[string]*profiles.Profile{"org": {URL: "http://127.0.0.1:1", Username: "pat", Token: "abhcli_x"}}, Current: "org"}
	_ = profiles.Save(filepath.Join(root, "profiles.json"), reg)
	_ = s.Remember(e.ID)
	if _, err := resolvePrincipal(signInFlags{}, false, os.Stdin, io.Discard); err != nil {
		t.Fatalf("offline: %v", err)
	}
	if p := currentPrincipal(); p.Kind != identity.Server || !p.Unverified {
		t.Fatalf("principal %+v", p)
	}
	if v := principalStart()["verified"]; v != false {
		t.Errorf("session.started verified = %v, want false", v)
	}
}

// A remembered server sign-in that holds nothing to sign in with is not a
// sign-in: under sign_in "server" the start exits 10, whether the profiles
// file, the profile or its token is missing, or the server refuses it.
func TestServerSignInWithoutCredentialFailsClosed(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	}))
	defer refusing.Close()
	for name, c := range map[string]struct {
		url     string
		profile *profiles.Profile // nil: no profiles file
		name    string
	}{
		"no profiles file": {"https://abhed.corp", nil, "org"},
		"no such profile":  {"https://abhed.corp", &profiles.Profile{URL: "https://abhed.corp", Token: "abhcli_x"}, "other"},
		"empty token":      {"https://abhed.corp", &profiles.Profile{URL: "https://abhed.corp"}, "org"},
	} {
		t.Run(name, func(t *testing.T) {
			signInTest(t)
			withManaged(t, `{"auth":{"client":{"sign_in":"server","server":"`+c.url+`"}}}`)
			s, _ := identity.Open()
			e, _ := s.AddServer(c.url, "mallory", "", c.name)
			root, _ := s.Root(e)
			if c.profile != nil {
				reg := &profiles.File{Profiles: map[string]*profiles.Profile{"org": c.profile}, Current: "org"}
				if err := profiles.Save(filepath.Join(root, "profiles.json"), reg); err != nil {
					t.Fatal(err)
				}
			}
			_ = s.Remember(e.ID)
			if code := enterPrincipal(signInFlags{}, false, os.Stdin, io.Discard); code != identity.ExitSignInRequired {
				t.Fatalf("exit %d, want 10; principal %+v", code, currentPrincipal())
			}
			if _, ok, _ := s.Remembered(nil); ok {
				t.Error("the credential-less sign-in is still remembered")
			}
		})
	}
}

// Where the organisation ships every record, a guest is refused.
func TestGuestRefusedWhereRecordsAreShipped(t *testing.T) {
	signInTest(t)
	withManaged(t, `{"record":{"ship":{"enabled":true}},"org_policy":{"url":"https://org.example"}}`)
	_, err := resolvePrincipal(signInFlags{guest: true}, false, os.Stdin, io.Discard)
	if !errors.Is(err, identity.ErrSignInRequired) || !strings.Contains(err.Error(), "ships every session's record") {
		t.Fatalf("err = %v", err)
	}
}

// Naming a guest's or an account's folder in ABHED_PERSON_ROOT is not
// signing in: under a required sign-in with guest off the start still
// exits 10, and nothing runs in the named folder.
func TestPersonRootFromTheEnvironmentIsNotASignIn(t *testing.T) {
	signInTest(t)
	withManaged(t, `{"auth":{"client":{"sign_in":"server","server":"https://abhed.corp","guest":false}}}`)
	h, _ := os.UserHomeDir()
	s, _ := identity.Open()
	e, _, err := s.Create("victim", "correct horse", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{filepath.Join(h, ".abhed", "guest", "planted"), filepath.Join(s.Dir(), e.ID)} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(home.EnvPerson, root)
		if code := enterPrincipal(signInFlags{}, false, os.Stdin, io.Discard); code != identity.ExitSignInRequired {
			t.Errorf("%s: exit %d, want 10", root, code)
		}
		if p, _ := home.Person(); p == root {
			t.Errorf("the run works in %s", root)
		}
	}
}

// Signing in over rpc or ACP is refused from inside an agent's command.
func TestSignInRefusedInsideAnAgentCommand(t *testing.T) {
	signInTest(t)
	old := inAgentCommand
	inAgentCommand = func() string { return "ABHED_SANDBOX is set" }
	t.Cleanup(func() { inAgentCommand = old })
	s, _ := identity.Open()
	_, _, _ = s.Create("a1", "correct horse", "", "")
	for _, m := range []string{"auth.signin", "auth.register", "auth.guest"} {
		if _, err := switchPrincipal(m, "a1", "correct horse", ""); err == nil || !strings.Contains(err.Error(), "agent's command") {
			t.Errorf("%s: %v", m, err)
		}
	}
	if e, _ := s.Get("a1"); e.Failed != 0 {
		t.Error("an agent's tries counted against the account")
	}
}

// A reset since the last sign-in is told on rpc and ACP too, once.
func TestResetNoticeOnEverySignInSurface(t *testing.T) {
	signInTest(t)
	s, _ := identity.Open()
	_, _, _ = s.Create("a1", "correct horse", "", "")
	if err := s.ResetAsOSUser("a1", "another horse", "pat", "mac"); err != nil {
		t.Fatal(err)
	}
	r := rpcAuth(rpcRequest{ID: "1", Method: "auth.signin", Username: "a1", Password: "another horse"}, false)
	if !strings.Contains(r.Notice, "without the recovery code") {
		t.Fatalf("rpc notice %q (%s)", r.Notice, r.Error)
	}
	endGuest()
	r = rpcAuth(rpcRequest{ID: "2", Method: "auth.signin", Username: "a1", Password: "another horse"}, false)
	if r.Notice != "" {
		t.Errorf("told twice: %q", r.Notice)
	}
}

// Each guest records as an owner of its own, so no guest reads another's
// sessions as its own.
func TestEachGuestIsItsOwnOwner(t *testing.T) {
	signInTest(t)
	if err := usePrincipal(identity.GuestPrincipal()); err != nil {
		t.Fatal(err)
	}
	first := cliUser()
	endGuest()
	if err := usePrincipal(identity.GuestPrincipal()); err != nil {
		t.Fatal(err)
	}
	second := cliUser()
	if !strings.HasPrefix(first, "guest:") || first == second || isMe(first) {
		t.Fatalf("guest owners %q and %q", first, second)
	}
}

// The server's answer at start decides: 401 or 403 end the sign-in; 5xx, 429
// and anything else are the server unavailable, and the run goes on
// unverified while the token is within its life, with no "no credential"
// text in what the person reads.
func TestServerAnswersAtStart(t *testing.T) {
	for _, c := range []struct {
		status  int
		forgets bool
	}{{401, true}, {403, true}, {429, false}, {500, false}, {502, false}, {503, false}, {418, false}} {
		t.Run(fmt.Sprint(c.status), func(t *testing.T) {
			signInTest(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(`{"error":"x"}`))
			}))
			defer srv.Close()
			withManaged(t, `{"auth":{"client":{"sign_in":"server","server":"`+srv.URL+`"}}}`)
			s, _ := identity.Open()
			e, _ := s.AddServer(srv.URL, "pat", "", "org")
			root, _ := s.Root(e)
			prof := &profiles.Profile{URL: srv.URL, Token: "abhcli_x", ExpiresAt: time.Now().Add(time.Hour)}
			_ = profiles.Save(filepath.Join(root, "profiles.json"), &profiles.File{Profiles: map[string]*profiles.Profile{"org": prof}, Current: "org"})
			_ = s.Remember(e.ID)
			var errOut strings.Builder
			code := enterPrincipal(signInFlags{}, false, os.Stdin, &errOut)
			_, still, _ := s.Remembered(nil)
			if c.forgets {
				if code != identity.ExitSignInRequired || still {
					t.Fatalf("exit %d, remembered %v", code, still)
				}
				if strings.Contains(errOut.String(), "no credential") {
					t.Errorf("the sentinel leaked: %q", errOut.String())
				}
				return
			}
			if code != 0 || !still || !currentPrincipal().Unverified {
				t.Fatalf("exit %d, remembered %v, principal %+v: %s", code, still, currentPrincipal(), errOut.String())
			}
		})
	}
}

// An unavailable server lets a required sign-in through only while its
// token is within its life.
func TestUnavailableServerWithAnExpiredToken(t *testing.T) {
	signInTest(t)
	withManaged(t, `{"auth":{"client":{"sign_in":"server","server":"http://127.0.0.1:1"}}}`)
	old := serverCheckWait
	serverCheckWait = 200 * time.Millisecond
	t.Cleanup(func() { serverCheckWait = old })
	s, _ := identity.Open()
	e, _ := s.AddServer("http://127.0.0.1:1", "pat", "", "org")
	root, _ := s.Root(e)
	prof := &profiles.Profile{URL: "http://127.0.0.1:1", Token: "abhcli_x", ExpiresAt: time.Now().Add(-time.Hour)}
	_ = profiles.Save(filepath.Join(root, "profiles.json"), &profiles.File{Profiles: map[string]*profiles.Profile{"org": prof}, Current: "org"})
	_ = s.Remember(e.ID)
	if code := enterPrincipal(signInFlags{}, false, os.Stdin, io.Discard); code != identity.ExitSignInRequired {
		t.Fatalf("exit %d", code)
	}
	if _, still, _ := s.Remembered(nil); !still {
		t.Error("an unreachable server ended the sign-in")
	}
}

// A child Abhed (an MCP server, a hook or command run unsandboxed) is told who is working in
// selectors that only narrow: a guest's child is a guest, and an account's
// child is that account only while it is remembered; a guest drops the
// variables that would reach an account's sign-ins or secrets.
func TestPrincipalIsHandedDownNarrowing(t *testing.T) {
	signInTest(t)
	t.Setenv(config.ProfilesEnv, "/elsewhere/profiles.json")
	t.Setenv(secrets.EnvFile, "/elsewhere/secrets.json")
	if err := usePrincipal(identity.GuestPrincipal()); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(EnvGuest) != "1" || os.Getenv(config.ProfilesEnv) != "" || os.Getenv(secrets.EnvFile) != "" {
		t.Fatalf("guest env: %q %q %q", os.Getenv(EnvGuest), os.Getenv(config.ProfilesEnv), os.Getenv(secrets.EnvFile))
	}
	// An MCP server is such a child: it gets the guest selector, and its
	// configured env cannot clear it or name an account.
	srv := strings.Join(mcp.ServerEnv([]string{EnvGuest + "=", EnvAccount + "=a1"}), "\n")
	if !strings.Contains(srv, EnvGuest+"=1") || strings.Contains(srv, EnvAccount+"=") {
		t.Fatalf("a guest's MCP server env:\n%s", srv)
	}
	endGuest()
	s, _ := identity.Open()
	e, _, _ := s.Create("a1", "correct horse", "", "")
	if err := usePrincipal(s.Principal(e)); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(EnvAccount) != "a1" || os.Getenv(EnvGuest) != "" {
		t.Fatalf("account env: %q %q", os.Getenv(EnvAccount), os.Getenv(EnvGuest))
	}
	endGuest()
	// The child: ABHED_ACCOUNT=a1 with a1 not remembered cannot run as a1.
	t.Setenv(EnvAccount, "a1")
	if code := enterPrincipal(signInFlags{}, false, os.Stdin, io.Discard); code != identity.ExitSignInRequired {
		t.Fatalf("a child named an account no one is signed in as: exit %d", code)
	}
}

// A server sign-in that is never kept, here refused by the organisation's
// rules after the login, has its token ended on the server; one that is kept
// does not.
func TestUnkeptServerSignInIsRevoked(t *testing.T) {
	signInTest(t)
	revoked := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cli/logout" {
			revoked <- r.Header.Get("Authorization")
		}
	}))
	defer srv.Close()
	s, _ := identity.Open()
	pending := func(token string) pendingServerSignIn {
		f, err := os.CreateTemp(s.Dir(), ".pending-profiles-*.json")
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		prof := &profiles.Profile{URL: srv.URL, Token: token, RevocationEndpoint: srv.URL + "/v1/cli/logout", Username: "pat"}
		if err := profiles.Save(f.Name(), &profiles.File{Profiles: map[string]*profiles.Profile{"org": prof}, Current: "org"}); err != nil {
			t.Fatal(err)
		}
		return pendingServerSignIn{path: f.Name(), filed: new(bool)}
	}
	refused := pending("tok-refused")
	if _, err := refused.file(s, identity.Rules{SignIn: config.SignInServer, Server: "https://elsewhere.example"}); err == nil {
		t.Fatal("a server the rules refuse was kept")
	}
	refused.discard()
	select {
	case got := <-revoked:
		if got != "Bearer tok-refused" {
			t.Fatalf("revoked %q", got)
		}
	default:
		t.Fatal("the refused sign-in's token stays valid on the server")
	}
	if _, err := os.Stat(refused.path); !os.IsNotExist(err) {
		t.Fatalf("the pending file is still there: %v", err)
	}

	kept := pending("tok-kept")
	if _, err := kept.file(s, identity.Rules{}); err != nil {
		t.Fatal(err)
	}
	kept.discard()
	select {
	case got := <-revoked:
		t.Fatalf("a kept sign-in's token was ended: %q", got)
	default:
	}
}
