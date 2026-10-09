package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// layerRig is a proxy-auth server whose sessions read a file the prompt
// names, with a policy layer per user.
type layerRig struct {
	srv    *Server
	h      http.Handler
	st     *agent.MemStore
	mu     sync.Mutex
	layers map[string]*PolicyLayer
	err    error
	asked  []SessionPolicyRequest
}

func newLayerRig(t *testing.T, mode string, managedKeys ...string) *layerRig {
	t.Helper()
	ws := t.TempDir()
	for _, f := range []string{"secret.txt", "ask.txt", "plain.txt"} {
		if err := os.WriteFile(filepath.Join(ws, f), []byte("contents of "+f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	call := func(f string) model.ToolCall {
		args, _ := json.Marshal(map[string]string{"path": filepath.Join(ws, f)})
		return model.ToolCall{ID: "c-" + f, Name: "read", Args: args}
	}
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	cfg.Permissions.Mode = mode
	cfg.ManagedKeys = managedKeys
	cfg.Managed = len(managedKeys) > 0
	r := &layerRig{st: agent.NewMemStore(), layers: map[string]*PolicyLayer{}}
	r.srv = New(Options{Workspace: ws, Config: cfg, Store: r.st, Redact: noRedact{},
		Adapter: promptAdapter{calls: map[string]model.ToolCall{
			"secret": call("secret.txt"), "ask": call("ask.txt"), "plain": call("plain.txt")}},
		Registry: tools.NewRegistry(tools.Read{}),
		SessionPolicy: func(_ context.Context, req SessionPolicyRequest) (*PolicyLayer, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.asked = append(r.asked, req)
			if r.err != nil {
				return nil, r.err
			}
			return r.layers[req.User], nil
		}})
	r.h = r.srv.Handler()
	return r
}

func (r *layerRig) set(l *PolicyLayer, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.layers["alice"], r.err = l, err
}

func (r *layerRig) do(user, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Abhed-User", user)
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w
}

// start runs one session for user to its first decision and returns its id.
func (r *layerRig) start(t *testing.T, user, prompt string) string {
	t.Helper()
	w := r.do(user, "POST", "/v1/sessions", `{"prompt":"`+prompt+`"}`)
	var out createResponse
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.SessionID == "" {
		t.Fatalf("no session for %s: %d %s", user, w.Code, w.Body)
	}
	r.decided(t, out.SessionID)
	return out.SessionID
}

// decided waits for the session's first decision and returns it.
func (r *layerRig) decided(t *testing.T, id string) (agent.EventType, map[string]string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		evs, _ := r.st.Events(id)
		for _, e := range evs {
			if e.Type == agent.EvActionApproved || e.Type == agent.EvActionDenied {
				var d map[string]string
				_ = json.Unmarshal(e.Payload, &d)
				return e.Type, d
			}
		}
	}
	t.Fatalf("session %s decided nothing", id)
	return "", nil
}

func (r *layerRig) layerEvents(id string) []map[string]any {
	evs, _ := r.st.Events(id)
	var out []map[string]any
	for _, e := range evs {
		if e.Type == agent.EvPolicyLayer {
			var p map[string]any
			_ = json.Unmarshal(e.Payload, &p)
			out = append(out, p)
		}
	}
	return out
}

var payLayer = &PolicyLayer{Name: "workspace pay", Revision: "3",
	Deny: []string{"read(**/secret.txt)"}, Ask: []string{"read(**/ask.txt)"}}

// A layer's deny rule refuses its owner's call even in bypass mode, the
// refusal names the layer, and the layer is on the record right after the
// session's start. Another person's session, with no layer, reads the file.
func TestPolicyLayerDeniesInEveryModeAndIsRecorded(t *testing.T) {
	r := newLayerRig(t, "bypass")
	r.set(payLayer, nil)
	id := r.start(t, "alice", "secret")
	typ, d := r.decided(t, id)
	if typ != agent.EvActionDenied || d["layer"] != "workspace pay" || d["rule"] != "read(**/secret.txt)" || d["step"] != "deny" {
		t.Fatalf("alice's read of secret.txt: %s %v", typ, d)
	}
	evs, _ := r.st.Events(id)
	if len(evs) < 2 || evs[0].Type != agent.EvSessionStarted || evs[1].Type != agent.EvPolicyLayer {
		t.Fatalf("the layer is not recorded right after session.started: %v", typesOf(evs))
	}
	got := r.layerEvents(id)[0]
	if got["layer"] != "workspace pay" || got["revision"] != "3" || got["sha256"] != payLayer.SHA256() {
		t.Fatalf("policy.layer: %v", got)
	}
	if typ, d := r.decided(t, r.start(t, "bob", "secret")); typ != agent.EvActionApproved || d["layer"] != "" {
		t.Fatalf("bob, outside the layer, was refused: %s %v", typ, d)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.asked) != 2 || r.asked[0].User != "alice" || r.asked[1].User != "bob" {
		t.Fatalf("the hook was asked for %+v", r.asked)
	}
}

// A layer's ask rule asks even in bypass mode; with nobody to answer, the
// call is refused, naming the layer.
func TestPolicyLayerAskHoldsInBypass(t *testing.T) {
	r := newLayerRig(t, "bypass")
	r.set(payLayer, nil)
	id, err := r.srv.StartSession(context.Background(), StartSpec{Prompt: "ask", User: "alice", Tenant: "default", Unattended: true})
	if err != nil {
		t.Fatal(err)
	}
	typ, d := r.decided(t, id)
	if typ != agent.EvActionDenied || d["step"] != "ask" || d["layer"] != "workspace pay" {
		t.Fatalf("an ask rule of the layer did not ask: %s %v", typ, d)
	}
}

// A layer that cannot be read, or does not validate, refuses the session
// with 503 and leaves nothing listed.
func TestPolicyLayerUnavailableRefusesTheSession(t *testing.T) {
	for name, set := range map[string]func(r *layerRig){
		"hook error":    func(r *layerRig) { r.set(nil, errors.New("store down")) },
		"broken rule":   func(r *layerRig) { r.set(&PolicyLayer{Name: "workspace pay", Deny: []string{"bash(rm"}}, nil) },
		"reserved name": func(r *layerRig) { r.set(&PolicyLayer{Name: "managed", Deny: []string{"bash(rm*)"}}, nil) },
		"no name":       func(r *layerRig) { r.set(&PolicyLayer{Deny: []string{"bash(rm*)"}}, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newLayerRig(t, "default")
			set(r)
			for _, body := range []string{`{"prompt":"plain"}`, `{"workbench":true}`} {
				w := r.do("alice", "POST", "/v1/sessions", body)
				if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "policy layer") {
					t.Fatalf("%s: %d %s", body, w.Code, w.Body)
				}
			}
			if w := r.do("alice", "GET", "/v1/sessions", ""); strings.Contains(w.Body.String(), `"id"`) {
				t.Fatalf("a refused session is listed: %s", w.Body)
			}
			if n := len(r.srv.running); n != 0 {
				t.Fatalf("%d sessions running", n)
			}
		})
	}
}

// Under a managed configuration that sets the permissions, a layer's allow
// rules are set aside and recorded so; its deny and ask rules still apply.
// Without one they apply, and /permissions shows each rule's layer.
func TestPolicyLayerAllowRulesYieldToManaged(t *testing.T) {
	l := &PolicyLayer{Name: "workspace pay", Deny: []string{"read(**/secret.txt)"}, Allow: []string{"bash(make*)"}}
	for _, locked := range []bool{false, true} {
		var keys []string
		if locked {
			keys = []string{"permissions.deny"}
		}
		r := newLayerRig(t, "default", keys...)
		r.set(l, nil)
		id := r.start(t, "alice", "secret")
		ev := r.layerEvents(id)[0]
		aside, _ := ev["set_aside"].([]any)
		allow, _ := ev["allow"].([]any)
		if locked != (len(aside) == 1 && len(allow) == 0) {
			t.Fatalf("locked %v: policy.layer %v", locked, ev)
		}
		w := r.do("alice", "GET", "/v1/sessions/"+id+"/permissions", "")
		var p permissionsResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatalf("permissions: %d %s", w.Code, w.Body)
		}
		var sawDeny, sawAllow bool
		for _, row := range p.Rules {
			sawDeny = sawDeny || row == permissionRow{Layer: "workspace pay", List: "deny", Rule: "read(**/secret.txt)"}
			sawAllow = sawAllow || row.Rule == "bash(make*)"
		}
		if !sawDeny || sawAllow == locked || (len(p.SetAside) == 1) != locked || p.Layer == nil || p.Layer.Name != "workspace pay" {
			t.Fatalf("locked %v: permissions %+v", locked, p)
		}
		if w := r.do("bob", "GET", "/v1/sessions/"+id+"/permissions", ""); w.Code != http.StatusNotFound {
			t.Fatalf("bob read alice's permissions: %d", w.Code)
		}
	}
}

// A continued session records its layer again only when it changed, and
// records a layer that has gone.
func TestLayerChange(t *testing.T) {
	a := &appliedLayer{PolicyLayer: *payLayer}
	ev := func(p map[string]any) agent.Event {
		b, _ := json.Marshal(p)
		return agent.Event{Type: agent.EvPolicyLayer, Payload: b}
	}
	if layerChange(nil, nil) != nil {
		t.Error("no layer, none recorded: nothing owed")
	}
	if got := layerChange(nil, a); got == nil || got["sha256"] != a.SHA256() {
		t.Errorf("a new layer is owed: %v", got)
	}
	if got := layerChange([]agent.Event{ev(a.record())}, a); got != nil {
		t.Errorf("an unchanged layer is recorded again: %v", got)
	}
	changed := &appliedLayer{PolicyLayer: PolicyLayer{Name: "workspace pay", Deny: []string{"bash(curl*)"}}}
	if got := layerChange([]agent.Event{ev(a.record())}, changed); got == nil || got["sha256"] != changed.SHA256() {
		t.Errorf("a changed layer is not owed: %v", got)
	}
	if got := layerChange([]agent.Event{ev(a.record())}, nil); got == nil || got["removed"] != true || got["layer"] != "workspace pay" {
		t.Errorf("a removed layer is not owed: %v", got)
	}
}

// Decide applies only the layer's deny and ask rules, and refuses what it
// cannot check against a deny rule.
func TestPolicyLayerDecide(t *testing.T) {
	l := PolicyLayer{Name: "workspace pay", Deny: []string{"bash(curl*)"}, Ask: []string{"bash(git push*)"}, Allow: []string{"bash(*)"}}
	cmd := func(c string) json.RawMessage { b, _ := json.Marshal(map[string]string{"command": c}); return b }
	for _, c := range []struct{ command, want string }{
		{"curl https://example.com", "deny"},
		{"ls && curl https://example.com", "deny"},
		{"eval \"$X\"", "deny"},
		{"git push origin main", "ask"},
		{"go test ./...", ""},
		{"rm -rf /", ""},
	} {
		if v := l.Decide("bash", cmd(c.command)); v.Decision != c.want {
			t.Errorf("%s: %+v, want %q", c.command, v, c.want)
		}
	}
	if v := (PolicyLayer{Name: "x", Deny: []string{"bash(oops"}}).Decide("bash", cmd("ls")); v.Decision != "deny" {
		t.Errorf("a broken layer did not refuse: %+v", v)
	}
}
