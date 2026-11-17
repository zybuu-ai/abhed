package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/identity"
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
		"_meta": map[string]any{"abhed": map[string]any{"username": "a1", "password": "nope nope nope"}}})
	if !strings.Contains(msg, "incorrect username or password") {
		t.Fatalf("wrong password: %s", msg)
	}
	var res map[string]any
	cl.ok("authenticate", map[string]any{"methodId": "abhed.account",
		"_meta": map[string]any{"abhed": map[string]any{"username": "a1", "password": "correct horse"}}}, &res)
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
		"_meta": map[string]any{"abhed": map[string]any{"username": "b2", "password": "another horse"}}}, nil)
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
