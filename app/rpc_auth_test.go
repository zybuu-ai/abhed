package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/internal/managed"
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
