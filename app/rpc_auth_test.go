package app

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/internal/managed"
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
	if p := currentPrincipal(); p.Kind != identity.Server {
		t.Fatalf("principal %+v", p)
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
