package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/k8s"
	"github.com/zybuu-ai/abhed/internal/machinegrant"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// firstUseApprover approves and counts what it is asked; always chooses the
// offered scope, as "always" does.
type firstUseApprover struct {
	mu     sync.Mutex
	asks   []policy.Result
	always bool
	no     bool
}

func (a *firstUseApprover) Approve(ctx context.Context, _ string, _ json.RawMessage, res policy.Result) (bool, error) {
	a.mu.Lock()
	a.asks = append(a.asks, res)
	a.mu.Unlock()
	if a.no {
		return false, nil
	}
	if a.always && res.Offer() != "" {
		NoteAnswer(ctx, Answer{By: ByReviewer, Granted: res.Offer()})
	}
	return true, nil
}

func (a *firstUseApprover) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.asks)
}

func machineGrants(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "machine-grants.json")
	old := machinegrant.Path
	machinegrant.Path = func() string { return p }
	t.Cleanup(func() { machinegrant.Path = old })
}

// fakeKube is a cluster over TLS and a kubeconfig whose context reaches it
// with a token, written to a throwaway folder.
func fakeKube(t *testing.T, token string) (string, *[]string) {
	t.Helper()
	var mu sync.Mutex
	seen := &[]string{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*seen = append(*seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"kind":"PodList","items":[{"metadata":{"name":"web-1","namespace":"default"}}]}`)
	}))
	t.Cleanup(srv.Close)
	kc := filepath.Join(t.TempDir(), "config")
	text := fmt.Sprintf(`current-context: lab
clusters:
- name: lab
  cluster:
    server: %s
    insecure-skip-tls-verify: true
contexts:
- name: lab
  context:
    cluster: lab
    user: me
users:
- name: me
  user:
    token: %s
`, srv.URL, token)
	if err := os.WriteFile(kc, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return kc, seen
}

func k8sGet(id string) scriptedTurn {
	return scriptedTurn{calls: []model.ToolCall{{ID: id, Name: "k8s_get", Args: json.RawMessage(`{"resource":"pods"}`)}}}
}

// A kubeconfig context is used as it is: the first read asks once, the
// rest of the session does not, and the token reaches neither the model,
// the record nor the approval.
func TestKubeconfigContextFirstUseAsksOnceAndKeepsTheTokenOut(t *testing.T) {
	machineGrants(t)
	const token = "kube-secret-token-9f8e7d6c"
	kc, seen := fakeKube(t, token)
	reg := tools.NewRegistry(k8s.GetTool{M: k8s.NewManager(k8s.Config{Kubeconfig: kc, FirstUseAll: true, KeptLogins: true})})
	sess, _ := tools.NewSession(t.TempDir())
	store := NewMemStore()
	rec := NewRecorder(store, "s1", "")
	ad := &scriptedAdapter{turns: []scriptedTurn{k8sGet("c1"), k8sGet("c2"), {text: "done"}}}
	appr := &firstUseApprover{}
	l := NewLoop(ad, reg, policy.New(policy.ModeDefault), appr, sess, rec, DefaultConfig())
	if _, err := l.Run(context.Background(), "list the pods"); err != nil {
		t.Fatal(err)
	}
	if appr.count() != 1 {
		t.Fatalf("asked %d times, want once", appr.count())
	}
	first := appr.asks[0]
	if !strings.Contains(first.Reason, "first use of kubeconfig context lab") || first.Keeps == "" || !tools.IsFirstUseScope(first.Offer()) {
		t.Fatalf("the ask: %+v", first)
	}
	if len(*seen) != 2 {
		t.Fatalf("the cluster saw %d requests", len(*seen))
	}
	evs, _ := store.Events("s1")
	for _, e := range evs {
		if strings.Contains(string(e.Payload), token) {
			t.Fatalf("the record holds the token in %s", e.Type)
		}
	}
	for i, req := range ad.gotRequests {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, token) {
				t.Fatalf("model call %d was sent the token", i)
			}
		}
		for _, tl := range req.Tools {
			if strings.Contains(tl.Description, token) {
				t.Fatal("a tool description carries the token")
			}
		}
	}
	if strings.Contains(first.Reason, token) {
		t.Fatal("the approval shows the token")
	}
	// Not kept: a "yes" covers this session only.
	if gs, _ := machinegrant.List(); len(gs) != 0 {
		t.Fatalf("kept %v without always", gs)
	}
}

func TestAlwaysKeepsTheContextForLaterSessions(t *testing.T) {
	machineGrants(t)
	kc, _ := fakeKube(t, "tok-a1b2c3d4e5f6")
	newLoop := func(appr Approver, turns ...scriptedTurn) *Loop {
		reg := tools.NewRegistry(k8s.GetTool{M: k8s.NewManager(k8s.Config{Kubeconfig: kc, FirstUseAll: true, KeptLogins: true})})
		sess, _ := tools.NewSession(t.TempDir())
		return NewLoop(&scriptedAdapter{turns: turns}, reg, policy.New(policy.ModeDefault), appr, sess,
			NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	}
	appr := &firstUseApprover{always: true}
	if _, err := newLoop(appr, k8sGet("c1"), scriptedTurn{text: "ok"}).Run(context.Background(), "pods"); err != nil {
		t.Fatal(err)
	}
	if gs, _ := machinegrant.List(); len(gs) != 1 || gs[0].Kind != machinegrant.KindK8sContext || gs[0].Name != "lab" {
		t.Fatalf("kept %+v", gs)
	}
	again := &firstUseApprover{}
	if _, err := newLoop(again, k8sGet("c1"), scriptedTurn{text: "ok"}).Run(context.Background(), "pods"); err != nil {
		t.Fatal(err)
	}
	if again.count() != 0 {
		t.Fatalf("a later session asked %d times after always", again.count())
	}
}

// An allow rule and bypass answer a first use as they answer any ask; plan
// and auto modes, which run reads unasked, still ask it; a deny stays a deny.
func TestFirstUseAndTheModes(t *testing.T) {
	machineGrants(t)
	kc, _ := fakeKube(t, "tok-z9y8x7w6v5")
	for _, tc := range []struct {
		name  string
		mode  policy.Mode
		allow string
		deny  string
		asks  int
	}{
		{"default", policy.ModeDefault, "", "", 1},
		{"plan", policy.ModePlan, "", "", 1},
		{"auto", policy.ModeAuto, "", "", 1},
		{"bypass", policy.ModeBypass, "", "", 0},
		{"allow rule", policy.ModeDefault, "k8s_get(context:*)", "", 0},
		{"deny rule", policy.ModeDefault, "", "k8s_get", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := tools.NewRegistry(k8s.GetTool{M: k8s.NewManager(k8s.Config{Kubeconfig: kc, FirstUseAll: true, KeptLogins: true})})
			sess, _ := tools.NewSession(t.TempDir())
			pol := policy.New(tc.mode)
			if tc.allow != "" {
				_ = pol.AddAllow(tc.allow)
			}
			if tc.deny != "" {
				_ = pol.AddDeny(tc.deny)
			}
			appr := &firstUseApprover{}
			l := NewLoop(&scriptedAdapter{turns: []scriptedTurn{k8sGet("c1"), {text: "ok"}}}, reg, pol, appr, sess,
				NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
			if _, err := l.Run(context.Background(), "pods"); err != nil {
				t.Fatal(err)
			}
			if appr.count() != tc.asks {
				t.Fatalf("asked %d times, want %d", appr.count(), tc.asks)
			}
		})
	}
}

// A credential helper runs a program: asked about by name, and never run
// when the person says no.
func TestExecHelperDoesNotRunWhenRefused(t *testing.T) {
	machineGrants(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	helper := filepath.Join(dir, "helper.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch "+marker+"\necho '{\"status\":{\"token\":\"t\"}}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	kc := filepath.Join(dir, "config")
	_ = os.WriteFile(kc, []byte(`current-context: cloud
clusters:
- name: c
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: cloud
  context:
    cluster: c
    user: u
users:
- name: u
  user:
    exec:
      command: `+helper+`
`), 0o600)
	reg := tools.NewRegistry(k8s.GetTool{M: k8s.NewManager(k8s.Config{Kubeconfig: kc})})
	sess, _ := tools.NewSession(t.TempDir())
	appr := &firstUseApprover{no: true}
	l := NewLoop(&scriptedAdapter{turns: []scriptedTurn{k8sGet("c1"), {text: "ok"}}}, reg, policy.New(policy.ModeDefault), appr, sess,
		NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	if _, err := l.Run(context.Background(), "pods"); err != nil {
		t.Fatal(err)
	}
	if appr.count() != 1 || !strings.Contains(appr.asks[0].Reason, helper) {
		t.Fatalf("asks %+v", appr.asks)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the credential helper ran though the person said no")
	}
}

// A pasted `oc login` reaches neither the model nor the record: both get a
// placeholder, the person is told, and the session holds the value for a
// login they approve.
func TestPastedLoginIsWithheldAndHeld(t *testing.T) {
	const tok = "sha256~Zq1cP3xYv8mN0aB4dE6fG7hI9jK2lM5nO8pQrStUvWx"
	reg := tools.NewRegistry(k8s.LoginTool{M: k8s.NewManager(k8s.Config{})})
	sess, _ := tools.NewSession(t.TempDir())
	store := NewMemStore()
	ad := &scriptedAdapter{turns: []scriptedTurn{{text: "I can log you in."}}}
	l := NewLoop(ad, reg, policy.New(policy.ModeDefault), AutoApprove{}, sess, NewRecorder(store, "s1", ""), DefaultConfig())
	msg := "connect: oc login --token=" + tok + " --server=https://api.lab.example:6443"
	if _, err := l.Run(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("s1")
	var said Message
	for _, e := range evs {
		if strings.Contains(string(e.Payload), tok) {
			t.Fatalf("the record holds the token in %s", e.Type)
		}
		if e.Type == EvUserMessage {
			_ = json.Unmarshal(e.Payload, &said)
		}
	}
	if said.Pasted != "P1:an OpenShift token" || !strings.Contains(said.Text, "[pasted:P1]") {
		t.Fatalf("user.message %+v", said)
	}
	sent := ""
	for _, m := range ad.gotRequests[0].Messages {
		sent += m.Content
	}
	if strings.Contains(sent, tok) {
		t.Fatal("the model was sent the token")
	}
	if !strings.Contains(sent, "P1 is an OpenShift token for the server https://api.lab.example:6443") || !strings.Contains(sent, "k8s_login") {
		t.Fatalf("the model was not told what was pasted: %q", sent)
	}
	p, ok := tools.PastedValue(sess, "P1")
	if !ok || p.Value != tok || p.Server != "https://api.lab.example:6443" {
		t.Fatalf("held %+v %v", p, ok)
	}
}

// Off the person's own machine a first use offers no "always", and an
// answer that grants nothing keeps nothing in the operator's state.
func TestFirstUseOffTheMachineKeepsNothing(t *testing.T) {
	machineGrants(t)
	kc, _ := fakeKube(t, "tok-server-0011223344")
	reg := tools.NewRegistry(k8s.GetTool{M: k8s.NewManager(k8s.Config{Kubeconfig: kc, FirstUseAll: true})})
	sess, _ := tools.NewSession(t.TempDir())
	appr := &firstUseApprover{always: true}
	l := NewLoop(&scriptedAdapter{turns: []scriptedTurn{k8sGet("c1"), {text: "ok"}}}, reg, policy.New(policy.ModeDefault), appr, sess,
		NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	if _, err := l.Run(context.Background(), "pods"); err != nil {
		t.Fatal(err)
	}
	if appr.count() != 1 || appr.asks[0].Offer() != "" || appr.asks[0].Keeps != "" {
		t.Fatalf("asks %+v", appr.asks)
	}
	if gs, _ := machinegrant.List(); len(gs) != 0 {
		t.Fatalf("kept %v off the person's machine", gs)
	}
}
