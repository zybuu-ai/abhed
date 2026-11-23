package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/machinegrant"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// grantsIn points the person's grants at a throwaway file.
func grantsIn(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "machine-grants.json")
	old := machinegrant.Path
	machinegrant.Path = func() string { return p }
	t.Cleanup(func() { machinegrant.Path = old })
}

func kubeconfigFile(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func twoContexts(server string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: dev
clusters:
- name: dev-cluster
  cluster:
    server: %s
- name: prod-cluster
  cluster:
    server: https://prod.example:6443
contexts:
- name: dev
  context:
    cluster: dev-cluster
    user: dev-user
- name: prod
  context:
    cluster: prod-cluster
    user: exec-user
users:
- name: dev-user
  user:
    token: kube-secret-token-123
- name: exec-user
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: gke-gcloud-auth-plugin
      args:
      - --use-adc
`, server)
}

func TestKubeconfigFilesMergeFirstWins(t *testing.T) {
	a := kubeconfigFile(t, twoContexts("https://dev.example:6443"))
	b := kubeconfigFile(t, `current-context: other
clusters:
- name: dev-cluster
  cluster:
    server: https://evil.example:6443
- name: other-cluster
  cluster:
    server: https://other.example:6443
contexts:
- name: dev
  context:
    cluster: dev-cluster
    user: dev-user
- name: other
  context:
    cluster: other-cluster
    user: dev-user
users:
- name: dev-user
  user:
    token: other-token
`)
	missing := filepath.Join(t.TempDir(), "absent")
	t.Setenv("KUBECONFIG", strings.Join([]string{missing, a, b}, string(os.PathListSeparator)))
	list, current, err := Contexts(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if current != "dev" {
		t.Fatalf("current-context %q, want the first file's", current)
	}
	names := map[string]string{}
	for _, c := range list {
		names[c.Name] = c.Server
	}
	if names["dev"] != "https://dev.example:6443" || names["other"] != "https://other.example:6443" || len(names) != 3 {
		t.Fatalf("contexts %v", names)
	}
	if list[0].Name != "dev" {
		t.Fatalf("the current context is not listed first: %v", list[0].Name)
	}
}

func TestAuthProviderAndBasicAuthAreRefusedWithReason(t *testing.T) {
	kc := kubeconfigFile(t, `current-context: gcp
clusters:
- name: c
  cluster:
    server: https://c.example
contexts:
- name: gcp
  context: {cluster: c, user: legacy}
- name: basic
  context:
    cluster: c
    user: pw
users:
- name: legacy
  user:
    auth-provider:
      name: gcp
- name: pw
  user:
    username: admin
    password: hunter2
`)
	_, err := Open(Config{Kubeconfig: kc, Context: "basic"})
	if err == nil || !strings.Contains(err.Error(), "basic auth") {
		t.Fatalf("basic auth: %v", err)
	}
	kc2 := kubeconfigFile(t, `current-context: gcp
clusters:
- name: c
  cluster:
    server: https://c.example
contexts:
- name: gcp
  context:
    cluster: c
    user: legacy
users:
- name: legacy
  user:
    auth-provider:
      name: gcp
`)
	_, err = Open(Config{Kubeconfig: kc2})
	if err == nil || !strings.Contains(err.Error(), "auth-provider") || !strings.Contains(err.Error(), "never writes") {
		t.Fatalf("auth-provider: %v", err)
	}
	m := NewManager(Config{Kubeconfig: kc2, FirstUseAll: true})
	if _, need := m.firstUse(newSession(t), json.RawMessage(`{"resource":"pods"}`)); need {
		t.Fatal("a refused context asked a first use instead of saying why it is refused")
	}
	if !strings.Contains((GetTool{M: m}).Description(), "not usable") {
		t.Fatal("the description offers a refused context as usable")
	}
}

func TestFirstUseIsAskedOnceKeptAndAskedAgainWhenTheServerChanges(t *testing.T) {
	grantsIn(t)
	kc := kubeconfigFile(t, twoContexts("https://dev.example:6443"))
	m := NewManager(Config{Kubeconfig: kc, FirstUseAll: true, KeptLogins: true})
	tool := GetTool{M: m}
	sess := newSession(t)
	args := json.RawMessage(`{"resource":"pods","context":"dev"}`)
	g, need := tool.FirstUse(sess, args)
	if !need || g.Name != "dev" || !strings.Contains(g.Reason, "https://dev.example:6443") || !tools.IsFirstUseScope(g.Scope) {
		t.Fatalf("first use: %v %+v", need, g)
	}
	if strings.Contains(g.Reason+g.What+g.Scope, "kube-secret-token-123") {
		t.Fatal("the first-use ask carries the token")
	}
	// The current context is asked about too when the call names none.
	if g2, need := tool.FirstUse(sess, json.RawMessage(`{"resource":"pods"}`)); !need || g2.Name != "dev" {
		t.Fatalf("current context: %v %+v", need, g2)
	}
	if err := tool.KeepFirstUse(g); err != nil {
		t.Fatal(err)
	}
	if _, need := tool.FirstUse(sess, args); need {
		t.Fatal("asked again after the person kept it")
	}
	if err := os.WriteFile(kc, []byte(twoContexts("https://moved.example:6443")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, need := tool.FirstUse(sess, args); !need {
		t.Fatal("a context whose server changed was not asked about again")
	}
}

// Where the configuration turned k8s on, only a context whose credential
// helper Abhed would run asks first; it names the program.
func TestExecHelperIsAskedAboutAndShown(t *testing.T) {
	grantsIn(t)
	kc := kubeconfigFile(t, twoContexts("https://dev.example:6443"))
	m := NewManager(Config{Kubeconfig: kc})
	sess := newSession(t)
	if _, need := m.firstUse(sess, json.RawMessage(`{"resource":"pods","context":"dev"}`)); need {
		t.Fatal("a token context asked where the configuration turned k8s on")
	}
	g, need := m.firstUse(sess, json.RawMessage(`{"resource":"pods","context":"prod"}`))
	if !need || !strings.Contains(g.Reason, "gke-gcloud-auth-plugin --use-adc") || !strings.Contains(g.Reason, "run") {
		t.Fatalf("exec context: %v %+v", need, g)
	}
	// Off the person's own machine nothing is kept: no "always" is offered.
	if g.Scope != "" {
		t.Fatalf("a server's first use offers always: %q", g.Scope)
	}
}

func TestClusterNamingAContextIsThatContextAndDeclaredWins(t *testing.T) {
	kc := kubeconfigFile(t, twoContexts("https://dev.example:6443"))
	m := NewManager(Config{Kubeconfig: kc})
	sess := newSession(t)
	out, changed, err := m.resolve(sess, json.RawMessage(`{"resource":"pods","cluster":"dev"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	var a map[string]any
	_ = json.Unmarshal(out, &a)
	if a["context"] != "dev" || a["cluster"] != nil || !strings.Contains(strings.Join(changed, ","), "cluster") {
		t.Fatalf("resolved %v (changed %v)", a, changed)
	}
	m2 := NewManager(Config{Kubeconfig: kc, Clusters: []LoginCluster{{Name: "dev", Server: "https://declared.example"}}})
	out, _, err = m2.resolve(sess, json.RawMessage(`{"resource":"pods","cluster":"dev"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		a = nil
		_ = json.Unmarshal(out, &a)
		if a["cluster"] != "dev" || a["context"] != nil {
			t.Fatalf("k8s.clusters did not win the clash: %v", a)
		}
	}
}

// A pasted `oc login` becomes a kept login on one approval: the server's
// self-signed certificate is pinned by its fingerprint, the token is stored
// as a secret, and a later session signs in with it by name alone.
func TestPastedLoginPinsStoresKeepsAndServesLaterSessions(t *testing.T) {
	grantsIn(t)
	const token = "sha256~pasted-token-value-0123456789abcdef"
	var mu sync.Mutex
	var auth []string
	srv, _ := tlsCluster(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"major":"1"}`)
			return
		}
		fmt.Fprint(w, `{"kind":"NamespaceList","items":[{"metadata":{"name":"web"}}]}`)
	}))
	store := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	newTool := func() LoginTool {
		m := NewManager(Config{Kubeconfig: filepath.Join(t.TempDir(), "none"), KeptLogins: true, Secret: store.Value})
		return LoginTool{M: m, Secret: store.Value, Store: store}
	}
	login := newTool()
	sess := newSession(t)
	id := tools.HoldPasted(sess, tools.Pasted{Kind: "an OpenShift token", Value: token, Server: srv.URL})
	raw := json.RawMessage(fmt.Sprintf(`{"cluster":"lab","pasted":%q}`, id))
	if err := login.PrecheckContext(context.Background(), sess, raw); err != nil {
		t.Fatal(err)
	}
	target := login.Target(sess, raw)
	if !strings.Contains(target, "SHA-256") || !strings.Contains(target, "K8S_LAB_TOKEN") || !strings.Contains(target, srv.URL) {
		t.Fatalf("the approval does not say what it does: %s", target)
	}
	if strings.Contains(target, token) {
		t.Fatal("the approval shows the token")
	}
	if sum, subject, pins := login.PinnedCertificate(sess, raw); !pins || !strings.Contains(target, sum) || subject == "" {
		t.Fatalf("pinned certificate %q %q %v", sum, subject, pins)
	}
	mu.Lock()
	if len(auth) != 0 {
		t.Fatalf("a request was sent before approval: %v", auth)
	}
	mu.Unlock()
	res := login.Run(context.Background(), sess, raw)
	if res.IsError || strings.Contains(res.Content, token) {
		t.Fatalf("pasted login: %+v", res)
	}
	if v, err := store.Value("K8S_LAB_TOKEN"); err != nil || v != token {
		t.Fatalf("stored %q, %v", v, err)
	}
	if kl, ok := machinegrant.Logins()["lab"]; !ok || kl.Server != srv.URL || kl.CAFingerprint == "" || kl.Secret != "K8S_LAB_TOKEN" {
		t.Fatalf("kept %+v", kl)
	}
	if _, held := tools.PastedValue(sess, id); held {
		t.Fatal("the pasted value is still held after it was stored")
	}

	// A later session: no paste, no token_secret, no allow rule to write.
	later := newSession(t)
	get := GetTool{M: newTool().M}
	if b := get.BoundSecrets(later, json.RawMessage(`{"cluster":"lab"}`)); len(b) != 1 || b[0] != "K8S_LAB_TOKEN" {
		t.Fatalf("bound secrets %v", b)
	}
	out := get.Run(context.Background(), later, json.RawMessage(`{"cluster":"lab","resource":"namespaces"}`))
	if out.IsError || !strings.Contains(out.Content, "web") {
		t.Fatalf("a kept login in a later session: %+v", out)
	}
}

func TestPastedLoginWithAWrongTokenStoresNothing(t *testing.T) {
	grantsIn(t)
	srv, _ := tlsCluster(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	store := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	m := NewManager(Config{Kubeconfig: filepath.Join(t.TempDir(), "none"), KeptLogins: true})
	login := LoginTool{M: m, Store: store, Secret: store.Value}
	sess := newSession(t)
	id := tools.HoldPasted(sess, tools.Pasted{Kind: "a cluster token", Value: "wrong-token-0123456789", Server: srv.URL})
	raw := json.RawMessage(fmt.Sprintf(`{"cluster":"lab","pasted":%q}`, id))
	if err := login.PrecheckContext(context.Background(), sess, raw); err != nil {
		t.Fatal(err)
	}
	if res := login.Run(context.Background(), sess, raw); !res.IsError {
		t.Fatalf("a wrong token logged in: %+v", res)
	}
	if names, _ := store.Names(); len(names) != 0 {
		t.Fatalf("stored %v", names)
	}
	if len(machinegrant.Logins()) != 0 {
		t.Fatal("kept a login that did not work")
	}
}

func TestPastedLoginIsRefusedWhereNothingCanBeKept(t *testing.T) {
	m := NewManager(Config{})
	sess := newSession(t)
	id := tools.HoldPasted(sess, tools.Pasted{Value: "tok-0123456789abcdef", Server: "https://a.example"})
	err := LoginTool{M: m}.PrecheckContext(context.Background(), sess, json.RawMessage(fmt.Sprintf(`{"cluster":"lab","pasted":%q}`, id)))
	if err == nil || !strings.Contains(err.Error(), "abhed secret set") {
		t.Fatalf("a server-side pasted login: %v", err)
	}
}

// Offered for k8s_login alone, the reads and writes appear once a session logs in.
func TestReadsAppearOnceASessionLogsIn(t *testing.T) {
	grantsIn(t)
	m := NewManager(Config{HiddenUntilLogin: true, KeptLogins: true})
	if !(GetTool{M: m}).Hidden() || !(ApplyTool{M: m}).Hidden() {
		t.Fatal("offered with nothing to reach")
	}
	// A login kept by another window counts as well.
	_ = machinegrant.Keep(machinegrant.Grant{Kind: machinegrant.KindK8sLogin, Name: "x", Identity: "i", Login: &machinegrant.Login{Server: "https://x", Secret: "S"}})
	if (GetTool{M: m}).Hidden() {
		t.Fatal("hidden with a kept login")
	}
	_, _ = machinegrant.Forget(machinegrant.KindK8sLogin, "x")
	if err := m.login(newSession(t), sessionCred{token: "t", cluster: LoginCluster{Name: "lab", Server: "https://lab.example"}}); err != nil {
		t.Fatal(err)
	}
	if (GetTool{M: m}).Hidden() || (ApplyTool{M: m}).Hidden() {
		t.Fatal("still hidden after a login")
	}
	if (GetTool{M: NewManager(Config{})}).Hidden() {
		t.Fatal("hidden where it was not asked to be")
	}
}
