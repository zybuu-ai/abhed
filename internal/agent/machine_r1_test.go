package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/k8s"
	"github.com/zybuu-ai/abhed/internal/machinegrant"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

const pinTok = "sha256~Zq1cP3xYv8mN0aB4dE6fG7hI9jK2lM5nO8pQrStUvWx"

// pastedLoginLoop is a session on the person's own machine, a self-signed
// cluster that takes pinTok, and a model that pastes nothing but calls
// k8s_login with the paste the person made.
func pastedLoginLoop(t *testing.T, pol *policy.Engine, appr Approver) (*Loop, *secrets.Store) {
	t.Helper()
	machineGrants(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+pinTok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"major":"1"}`)
	}))
	t.Cleanup(srv.Close)
	store := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	m := k8s.NewManager(k8s.Config{Kubeconfig: filepath.Join(t.TempDir(), "none"), KeptLogins: true, Secret: store.Value})
	reg := tools.NewRegistry(k8s.LoginTool{M: m, Secret: store.Value, Store: store})
	sess, _ := tools.NewSession(t.TempDir())
	ad := &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{{ID: "c1", Name: "k8s_login", Args: json.RawMessage(`{"cluster":"lab","pasted":"P1"}`)}}},
		{text: "done"},
	}}
	l := NewLoop(ad, reg, pol, appr, sess, NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	if _, err := l.Run(context.Background(), "oc login --token="+pinTok+" --server="+srv.URL); err != nil {
		t.Fatal(err)
	}
	return l, store
}

// Pinning a certificate no CA vouches for is put to a person, with its
// fingerprint, in bypass and under an allow rule alike; with no one to
// ask it is refused and nothing is stored or kept.
func TestAPinningLoginIsAlwaysPutToAPerson(t *testing.T) {
	for _, tc := range []struct {
		name string
		pol  func() *policy.Engine
	}{
		{"bypass", func() *policy.Engine { return policy.New(policy.ModeBypass) }},
		{"allow rule", func() *policy.Engine { p := policy.New(policy.ModeDefault); _ = p.AddAllow("k8s_login"); return p }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			appr := &firstUseApprover{}
			_, store := pastedLoginLoop(t, tc.pol(), appr)
			if appr.count() != 1 || appr.asks[0].PinSHA256 == "" || appr.asks[0].Offer() != "" ||
				!strings.Contains(appr.asks[0].Reason, appr.asks[0].PinSHA256) {
				t.Fatalf("asks %+v", appr.asks)
			}
			if _, ok := machinegrant.Logins()["lab"]; !ok {
				t.Fatal("the approved login was not kept")
			}
			if v, _ := store.Value("K8S_LAB_TOKEN"); v != pinTok {
				t.Fatal("the approved token was not stored")
			}
		})
	}
	t.Run("nobody to ask", func(t *testing.T) {
		_, store := pastedLoginLoop(t, policy.New(policy.ModeBypass), AutoApprove{Yes: true})
		if len(machinegrant.Logins()) != 0 {
			t.Fatal("a pin no one saw was kept")
		}
		if names, _ := store.Names(); len(names) != 0 {
			t.Fatalf("stored %v", names)
		}
	})
}

// A deny rule on the secret a pasted login would store into refuses it
// before anything is read, sent or stored.
func TestADenyRuleOnStoreAsRefusesThePastedLogin(t *testing.T) {
	pol := policy.New(policy.ModeDefault)
	_ = pol.AddDeny("secret(K8S_LAB_TOKEN)")
	appr := &firstUseApprover{}
	_, store := pastedLoginLoop(t, pol, appr)
	if appr.count() != 0 {
		t.Fatalf("asked %d times", appr.count())
	}
	if names, _ := store.Names(); len(names) != 0 {
		t.Fatalf("stored %v", names)
	}
}

// A deny rule on a kept login's bound secret refuses the call, and the
// secret is never read.
func TestADenyRuleRefusesABoundSecret(t *testing.T) {
	machineGrants(t)
	if err := machinegrant.Keep(machinegrant.Grant{Kind: machinegrant.KindK8sLogin, Name: "lab", Identity: "i",
		Login: &machinegrant.Login{Server: "https://127.0.0.1:1", Secret: "K8S_LAB_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	read := 0
	secret := func(string) (string, error) { read++; return "tok", nil }
	reg := tools.NewRegistry(k8s.GetTool{M: k8s.NewManager(k8s.Config{KeptLogins: true, Secret: secret})})
	sess, _ := tools.NewSession(t.TempDir())
	pol := policy.New(policy.ModeBypass)
	_ = pol.AddDeny("secret(K8S_LAB_TOKEN)")
	store := NewMemStore()
	l := NewLoop(&scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{{ID: "c1", Name: "k8s_get", Args: json.RawMessage(`{"cluster":"lab","resource":"pods"}`)}}},
		{text: "ok"}}}, reg, pol, AutoApprove{Yes: true}, sess, NewRecorder(store, "s", ""), DefaultConfig())
	if _, err := l.Run(context.Background(), "pods"); err != nil {
		t.Fatal(err)
	}
	if read != 0 {
		t.Fatalf("the denied secret was read %d times", read)
	}
	evs, _ := store.Events("s")
	denied := false
	for _, e := range evs {
		denied = denied || e.Type == EvActionDenied && strings.Contains(string(e.Payload), "secret K8S_LAB_TOKEN is denied")
	}
	if !denied {
		t.Fatal("the call was not denied for its bound secret")
	}
}

// A held value is withheld wherever it appears again: a later message, in
// the record and the model input alike, and a tool's output.
func TestAHeldValueIsWithheldWhereverItAppearsAgain(t *testing.T) {
	const tok = "q8Zt3kLm9PxR2vWn7YbC4dFg"
	echo := echoTool{}
	sess, _ := tools.NewSession(t.TempDir())
	store := NewMemStore()
	ad := &scriptedAdapter{turns: []scriptedTurn{{text: "ok"},
		{calls: []model.ToolCall{{ID: "c1", Name: "echo", Args: json.RawMessage(`{}`)}}}, {text: "ok"}}}
	l := NewLoop(ad, tools.NewRegistry(echo), policy.New(policy.ModeBypass), AutoApprove{Yes: true}, sess, NewRecorder(store, "s", ""), DefaultConfig())
	if _, err := l.Run(context.Background(), "oc login --token="+tok+" --server=https://api.x.example:6443"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(context.Background(), "the token I pasted was "+tok); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("s")
	for _, e := range evs {
		if strings.Contains(string(e.Payload), tok) {
			t.Fatalf("the record holds the value in %s", e.Type)
		}
	}
	for i, req := range ad.gotRequests {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, tok) {
				t.Fatalf("model call %d was sent the value", i)
			}
		}
	}
	if got := l.Withheld("again " + tok); strings.Contains(got, tok) {
		t.Fatalf("Withheld kept it: %q", got)
	}
}

// echoTool prints the held value, as a command that reads a file holding it does.
type echoTool struct{}

func (echoTool) Name() string            { return "echo" }
func (echoTool) Description() string     { return "echo" }
func (echoTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (echoTool) Mutates() bool           { return false }
func (echoTool) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	return tools.Result{Content: "token: q8Zt3kLm9PxR2vWn7YbC4dFg"}
}

// A corrected re-paste is held under its own id with its own server, and
// the note names that server.
func TestACorrectedRepasteNamesTheRightServer(t *testing.T) {
	const tok = "q8Zt3kLm9PxR2vWn7YbC4dFg"
	sess, _ := tools.NewSession(t.TempDir())
	ad := &scriptedAdapter{turns: []scriptedTurn{{text: "ok"}, {text: "ok"}}}
	l := NewLoop(ad, tools.NewRegistry(), policy.New(policy.ModeDefault), AutoApprove{}, sess, NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	_, _ = l.Run(context.Background(), "oc login --token="+tok+" --server=https://api.typo.example:6443")
	_, _ = l.Run(context.Background(), "sorry: oc login --token="+tok+" --server=https://api.right.example:6443")
	p2, ok := tools.PastedValue(sess, "P2")
	if !ok || p2.Server != "https://api.right.example:6443" {
		t.Fatalf("P2 = %+v %v", p2, ok)
	}
	if p1, _ := tools.PastedValue(sess, "P1"); p1.Server != "https://api.typo.example:6443" {
		t.Fatalf("P1 = %+v", p1)
	}
	last := ad.gotRequests[len(ad.gotRequests)-1].Messages
	if !strings.Contains(last[len(last)-1].Content, "P2 is an OpenShift token for the server https://api.right.example:6443") &&
		!strings.Contains(last[len(last)-1].Content, "P2 is a cluster token for the server https://api.right.example:6443") {
		t.Fatalf("the note: %q", last[len(last)-1].Content)
	}
}

// What a history file, a session row or an editor's request keeps holds no
// pasted credential's shape.
func TestWithheldTextHoldsNoPastedShape(t *testing.T) {
	got := WithheldText(nil, "oc login --token="+pinTok+" --server=https://api.x.example:6443")
	if strings.Contains(got, pinTok) || !strings.Contains(got, "[pasted credential]") {
		t.Fatalf("%q", got)
	}
}

// A message that was not delivered is kept with what it held withheld and
// named, and a paste with no session to hold it is still named.
func TestDroppedAndUnheldPastesAreNamed(t *testing.T) {
	sess, _ := tools.NewSession(t.TempDir())
	l := NewLoop(&scriptedAdapter{}, tools.NewRegistry(), policy.New(policy.ModeDefault), AutoApprove{}, sess, NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	d := l.dropped(DroppedMessage{Text: "oc login --token=" + pinTok + " --server=https://a.example", Steered: true})
	if strings.Contains(d.Text, pinTok) || d.Pasted != ":an OpenShift token" || !d.Steered {
		t.Fatalf("dropped %+v", d)
	}
	nobody := &Loop{}
	text, pasted, _ := nobody.withholdPasted("oc login --token=" + pinTok + " --server=https://a.example")
	if strings.Contains(text, pinTok) || pasted != ":an OpenShift token" {
		t.Fatalf("unheld: %q %q", text, pasted)
	}
}

// A value the session holds from a paste, printed by a background shell, is
// withheld from its output, its end notice, the listing, the record and what
// the model is sent, as a stored secret's value is.
func TestABackgroundShellPrintingAHeldValueIsWithheld(t *testing.T) {
	const held = "q8Zt3kLm9PxR2vWn7YbC4dFg"
	a := &stepAdapter{steps: []func(model.Request) scriptedTurn{
		func(model.Request) scriptedTurn {
			return scriptedTurn{calls: []model.ToolCall{{ID: "c1", Name: "bash", Args: json.RawMessage(
				`{"command":"printf 'held=q8Zt3kLm9P''xR2vWn7YbC4dFg'","description":"print it","run_in_background":true}`)}}}
		},
		func(req model.Request) scriptedTurn {
			return scriptedTurn{calls: []model.ToolCall{{ID: "c2", Name: "shell_output", Args: json.RawMessage(
				`{"shell_id":"` + lastShellID(req) + `","wait_ms":5000}`)}}}
		},
	}}
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, a)
	if id := tools.HoldPasted(r.l.Session, tools.Pasted{Kind: "a cluster token", Value: held}); id != "P1" {
		t.Fatalf("held as %q", id)
	}
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.l.Run(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	for _, e := range r.events(t) {
		if strings.Contains(string(e.Payload), held) {
			t.Fatalf("the record holds the value in %s: %s", e.Type, e.Payload)
		}
	}
	for _, ti := range r.l.Background.Tasks() {
		if strings.Contains(ti.LastLine, held) {
			t.Fatalf("listing: %+v", ti)
		}
	}
	for _, n := range r.l.Background.peekNotices() {
		if strings.Contains(n.Content, held) {
			t.Fatal("the end notice holds the value")
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	named := false
	for _, req := range a.reqs {
		for _, m := range req.Messages {
			if m.Role == model.RoleAssistant {
				continue
			}
			if strings.Contains(m.Content, held) {
				t.Fatalf("the model was sent the value: %q", m.Content)
			}
			named = named || strings.Contains(m.Content, "held=[pasted:P1]")
		}
	}
	if !named {
		t.Fatal("the shell's output was not seen withheld")
	}
}

// An error a run returns, and a folder's memory, are withheld the same way.
func TestRedactErrWithholdsAHeldValue(t *testing.T) {
	sess, _ := tools.NewSession(t.TempDir())
	l := NewLoop(&scriptedAdapter{}, tools.NewRegistry(), policy.New(policy.ModeDefault), AutoApprove{}, sess, NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	tools.HoldPasted(sess, tools.Pasted{Value: "q8Zt3kLm9PxR2vWn7YbC4dFg"})
	err := l.redactErr(fmt.Errorf("provider said q8Zt3kLm9PxR2vWn7YbC4dFg is bad"))
	if strings.Contains(err.Error(), "q8Zt3kLm9PxR2vWn7YbC4dFg") || !strings.Contains(err.Error(), "[pasted:P1]") {
		t.Fatalf("%v", err)
	}
}

// A dropped message names the held values it withheld, and the history
// file's redaction withholds a bare re-mention of one.
func TestDroppedNamesHeldValuesAndHistoryWithholdsThem(t *testing.T) {
	const held = "q8Zt3kLm9PxR2vWn7YbC4dFg"
	sess, _ := tools.NewSession(t.TempDir())
	l := NewLoop(&scriptedAdapter{}, tools.NewRegistry(), policy.New(policy.ModeDefault), AutoApprove{}, sess, NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	tools.HoldPasted(sess, tools.Pasted{Kind: "a cluster token", Value: held})
	d := l.dropped(DroppedMessage{Text: "it was " + held})
	if strings.Contains(d.Text, held) || d.Pasted != "P1:a cluster token" {
		t.Fatalf("dropped %+v", d)
	}
	if got := WithheldFor(nil, sess, "again "+held); strings.Contains(got, held) || !strings.Contains(got, "[pasted:P1]") {
		t.Fatalf("history %q", got)
	}
}

// Where the organisation's configuration sets k8s, a pasted login is still
// withheld, and the login the model makes is refused with the reason, never
// as an unknown tool.
func TestAPastedLoginRefusedByTheOrganisationSaysWhy(t *testing.T) {
	why := "your organisation's configuration (/etc/abhed/config.json) sets k8s, so only what it configures is offered here; ask your administrator"
	reg := tools.NewRegistry(k8s.LoginTool{M: k8s.NewManager(k8s.Config{Off: why, HiddenUntilLogin: true})})
	sess, _ := tools.NewSession(t.TempDir())
	store := NewMemStore()
	ad := &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{{ID: "c1", Name: "k8s_login", Args: json.RawMessage(`{"cluster":"lab","pasted":"P1"}`)}}},
		{text: "ok"},
	}}
	l := NewLoop(ad, reg, policy.New(policy.ModeDefault), &firstUseApprover{}, sess, NewRecorder(store, "s", ""), DefaultConfig())
	if _, err := l.Run(context.Background(), "oc login --token="+pinTok+" --server=https://api.x.example:6443"); err != nil {
		t.Fatal(err)
	}
	sent := ""
	for _, m := range ad.gotRequests[len(ad.gotRequests)-1].Messages {
		sent += m.Content
	}
	if strings.Contains(sent, pinTok) || strings.Contains(sent, "nknown tool") || !strings.Contains(sent, "ask your administrator") {
		t.Fatalf("the model was told: %q", sent)
	}
	// The note on the paste already says logins are off, and why.
	first := ""
	for _, m := range ad.gotRequests[0].Messages {
		first += m.Content
	}
	if !strings.Contains(first, "Cluster logins are off on this machine") || !strings.Contains(first, "ask your administrator") ||
		strings.Contains(first, "offer to log in") {
		t.Fatalf("the paste's note: %q", first)
	}
}

// A login pasted again whose token is already stored is still read as a
// paste: the session holds the real token, not [secret:NAME], so the login
// it asks for can never send the placeholder.
func TestARepastedStoredTokenIsStillAPaste(t *testing.T) {
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("K8S_LAB_TOKEN", pinTok); err != nil {
		t.Fatal(err)
	}
	sess, _ := tools.NewSession(t.TempDir())
	rec := NewRecorder(NewMemStore(), "s", "")
	rec.Redact = vault.Redactor()
	ad := &scriptedAdapter{turns: []scriptedTurn{{text: "ok"}}}
	l := NewLoop(ad, tools.NewRegistry(), policy.New(policy.ModeDefault), AutoApprove{}, sess, rec, DefaultConfig())
	if _, err := l.Run(context.Background(), "oc login --token="+pinTok+" --server=https://api.lab.example:6443"); err != nil {
		t.Fatal(err)
	}
	p, ok := tools.PastedValue(sess, "P1")
	if !ok || p.Value != pinTok || p.Server != "https://api.lab.example:6443" {
		t.Fatalf("held %+v %v", p, ok)
	}
	for _, m := range ad.gotRequests[0].Messages {
		if strings.Contains(m.Content, pinTok) {
			t.Fatal("the model was sent the token")
		}
	}
}
