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
	// A login kept by another window since does not change what this
	// session offers, so its prompt cache holds; the next session sees it.
	_ = machinegrant.Keep(machinegrant.Grant{Kind: machinegrant.KindK8sLogin, Name: "x", Identity: "i", Login: &machinegrant.Login{Server: "https://x", Secret: "S"}})
	if !(GetTool{M: m}).Hidden() || strings.Contains((LoginTool{M: m}).Description(), "Clusters: x") {
		t.Fatal("what this session offers changed mid-session")
	}
	next := NewManager(Config{HiddenUntilLogin: true, KeptLogins: true})
	if (GetTool{M: next}).Hidden() || !strings.Contains((LoginTool{M: next}).Description(), "Clusters: x") {
		t.Fatal("the next session does not see the kept login")
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

// A guest's pasted login holds for the session only: the approval says so,
// and nothing is stored or kept.
func TestAGuestsPastedLoginIsForTheSessionOnly(t *testing.T) {
	grantsIn(t)
	const token = "sha256~guest-token-value-0123456789abcdef"
	srv, _ := tlsCluster(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"kind":"NamespaceList","items":[{"metadata":{"name":"web"}}]}`)
	}))
	store := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	m := NewManager(Config{Kubeconfig: filepath.Join(t.TempDir(), "none"), SessionLogins: true, Secret: store.Value})
	login := LoginTool{M: m, Secret: store.Value, Store: store}
	sess := newSession(t)
	id := tools.HoldPasted(sess, tools.Pasted{Kind: "an OpenShift token", Value: token, Server: srv.URL})
	raw := json.RawMessage(fmt.Sprintf(`{"cluster":"lab","pasted":%q}`, id))
	if err := login.PrecheckContext(context.Background(), sess, raw); err != nil {
		t.Fatal(err)
	}
	if target := login.Target(sess, raw); !strings.Contains(target, "for this session only") || !strings.Contains(target, "guest") {
		t.Fatalf("the approval does not say it is for the session only: %s", target)
	}
	if res := login.Run(context.Background(), sess, raw); res.IsError || !strings.Contains(res.Content, "nothing is stored or kept") {
		t.Fatalf("%+v", res)
	}
	if names, _ := store.Names(); len(names) != 0 {
		t.Fatalf("stored %v", names)
	}
	if len(machinegrant.Logins()) != 0 {
		t.Fatal("a guest's login was kept")
	}
	out := GetTool{M: m}.Run(context.Background(), sess, json.RawMessage(`{"cluster":"lab","resource":"namespaces"}`))
	if out.IsError || !strings.Contains(out.Content, "web") {
		t.Fatalf("the session's login does not work: %+v", out)
	}
}

// Pasting a kept token again changes nothing and sends nothing; a new token
// for a kept cluster replaces its token after the approval.
func TestRepastingAKeptLogin(t *testing.T) {
	grantsIn(t)
	const old, fresh = "sha256~old-token-value-0123456789abcdef", "sha256~new-token-value-0123456789abcdef"
	var mu sync.Mutex
	var auth []string
	srv, _ := tlsCluster(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		fmt.Fprint(w, `{"major":"1"}`)
	}))
	store := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	m := NewManager(Config{Kubeconfig: filepath.Join(t.TempDir(), "none"), KeptLogins: true, Secret: store.Value})
	login := LoginTool{M: m, Secret: store.Value, Store: store}
	sess := newSession(t)
	// Kept once.
	id := tools.HoldPasted(sess, tools.Pasted{Kind: "an OpenShift token", Value: old, Server: srv.URL})
	raw := json.RawMessage(fmt.Sprintf(`{"cluster":"lab","pasted":%q}`, id))
	if err := login.PrecheckContext(context.Background(), sess, raw); err != nil {
		t.Fatal(err)
	}
	if res := login.Run(context.Background(), sess, raw); res.IsError {
		t.Fatalf("%+v", res)
	}
	mu.Lock()
	sent := len(auth)
	mu.Unlock()
	// The same token again: already kept, nothing sent.
	again := newSession(t)
	id = tools.HoldPasted(again, tools.Pasted{Kind: "an OpenShift token", Value: old, Server: srv.URL})
	err := login.PrecheckContext(context.Background(), again, json.RawMessage(fmt.Sprintf(`{"cluster":"lab2","pasted":%q}`, id)))
	if err == nil || !strings.Contains(err.Error(), "already kept") || !strings.Contains(err.Error(), `"lab"`) {
		t.Fatalf("the same token again: %v", err)
	}
	mu.Lock()
	if len(auth) != sent {
		t.Fatal("a request was sent for a token already kept")
	}
	mu.Unlock()
	// Another token for the kept cluster: it replaces the kept one.
	id = tools.HoldPasted(again, tools.Pasted{Kind: "an OpenShift token", Value: fresh, Server: srv.URL})
	raw = json.RawMessage(fmt.Sprintf(`{"cluster":"lab","pasted":%q}`, id))
	if err := login.PrecheckContext(context.Background(), again, raw); err != nil {
		t.Fatalf("a new token for a kept cluster: %v", err)
	}
	if target := login.Target(again, raw); !strings.Contains(target, "replaces the token kept for cluster lab") {
		t.Fatalf("the approval: %s", target)
	}
	if res := login.Run(context.Background(), again, raw); res.IsError {
		t.Fatalf("%+v", res)
	}
	if v, _ := store.Value("K8S_LAB_TOKEN"); v != fresh {
		t.Fatalf("the kept token is %q", v)
	}
	mu.Lock()
	last := auth[len(auth)-1]
	mu.Unlock()
	if last != "Bearer "+fresh {
		t.Fatalf("sent %q", last)
	}
}

// A placeholder is never sent as a credential.
func TestAMaskedTokenIsNeverSent(t *testing.T) {
	hits := 0
	srv, _ := tlsCluster(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	for _, tok := range []string{"[secret:K8S_LAB_TOKEN]", "x[pasted:P1]", "[pasted credential]"} {
		if _, err := OpenLogin(LoginCluster{Name: "lab", Server: srv.URL, InsecureSkipTLSVerify: true}, "", tok, ""); err == nil ||
			!strings.Contains(err.Error(), "placeholder") {
			t.Fatalf("%q: %v", tok, err)
		}
	}
	c := &Cluster{Name: "lab", Server: srv.URL, bearer: "[secret:K8S_LAB_TOKEN]", client: srv.Client()}
	if _, err := c.Do(context.Background(), "GET", "/version", nil); err == nil || hits != 0 {
		t.Fatalf("a masked bearer was sent: %v (%d)", err, hits)
	}
}

// pasteRig is a self-signed cluster that takes any token, a store, a login
// tool, and a function that pastes a token and runs the login.
func pasteRig(t *testing.T) (*secrets.Store, LoginTool, string, func(cluster, token string) (json.RawMessage, *tools.Session)) {
	t.Helper()
	grantsIn(t)
	srv, _ := tlsCluster(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"major":"1"}`) }))
	store := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	m := NewManager(Config{Kubeconfig: filepath.Join(t.TempDir(), "none"), KeptLogins: true, Secret: store.Value})
	login := LoginTool{M: m, Secret: store.Value, Store: store}
	paste := func(cluster, token string) (json.RawMessage, *tools.Session) {
		sess := newSession(t)
		id := tools.HoldPasted(sess, tools.Pasted{Kind: "an OpenShift token", Value: token, Server: srv.URL})
		return json.RawMessage(fmt.Sprintf(`{"cluster":%q,"pasted":%q}`, cluster, id)), sess
	}
	return store, login, srv.URL, paste
}

func keepLogin(t *testing.T, cluster, server, secret string, created bool) {
	t.Helper()
	if err := machinegrant.Keep(machinegrant.Grant{Kind: machinegrant.KindK8sLogin, Name: cluster, Identity: cluster,
		Login: &machinegrant.Login{Server: server, Secret: secret, Created: created}}); err != nil {
		t.Fatal(err)
	}
}

// A new token for a kept cluster never overwrites a secret it does not own:
// one two clusters share, or one that held the value before the login was
// kept. It is stored under a fresh name and the login is rebound to it.
func TestARepasteNeverOverwritesASecretItDoesNotOwn(t *testing.T) {
	for _, tc := range []struct {
		name, secret string
		created      bool
		other        bool
	}{
		{"shared by two clusters", "SHARED_TOKEN", true, true},
		{"held before the login was kept", "OCP_TOKEN", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, login, server, paste := pasteRig(t)
			if err := store.Set(tc.secret, "sha256~old-shared-token-0123456789"); err != nil {
				t.Fatal(err)
			}
			keepLogin(t, "lab", server, tc.secret, tc.created)
			if tc.other {
				keepLogin(t, "lab2", "https://other.example:6443", tc.secret, true)
			}
			raw, sess := paste("lab", "sha256~new-token-value-0123456789abcdef")
			if b := login.BoundSecrets(sess, raw); len(b) != 1 || b[0] != "K8S_LAB_TOKEN" {
				t.Fatalf("the deny rules would be checked on %v", b)
			}
			if err := login.PrecheckContext(context.Background(), sess, raw); err != nil {
				t.Fatal(err)
			}
			if res := login.Run(context.Background(), sess, raw); res.IsError {
				t.Fatalf("%+v", res)
			}
			if v, _ := store.Value(tc.secret); v != "sha256~old-shared-token-0123456789" {
				t.Fatalf("%s was overwritten: %q", tc.secret, v)
			}
			if v, _ := store.Value("K8S_LAB_TOKEN"); v != "sha256~new-token-value-0123456789abcdef" {
				t.Fatalf("the new token is %q", v)
			}
			if kl := machinegrant.Logins()["lab"]; kl.Secret != "K8S_LAB_TOKEN" || !kl.Created {
				t.Fatalf("lab is kept with %+v", kl)
			}
			if tc.other {
				if kl := machinegrant.Logins()["lab2"]; kl.Secret != tc.secret {
					t.Fatalf("lab2 lost its secret: %+v", kl)
				}
			}
		})
	}
}

// Replacing the token a paste made for a login alone writes that secret,
// and a deny rule on it is checked: BoundSecrets names it.
func TestReplacingAPasteMadeSecretNamesItForTheDenyRules(t *testing.T) {
	store, login, server, paste := pasteRig(t)
	if err := store.Set("K8S_LAB_TOKEN", "sha256~old-token-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	keepLogin(t, "lab", server, "K8S_LAB_TOKEN", true)
	raw, sess := paste("lab", "sha256~new-token-0123456789abcdef")
	if b := login.BoundSecrets(sess, raw); len(b) != 1 || b[0] != "K8S_LAB_TOKEN" {
		t.Fatalf("BoundSecrets %v", b)
	}
}

// A login naming a stored secret the kept login already binds is already
// kept: nothing is sent.
func TestALoginNamingTheKeptSecretIsAlreadyKept(t *testing.T) {
	store, login, server, _ := pasteRig(t)
	if err := store.Set("OCP_TOKEN", "sha256~tok-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	keepLogin(t, "lab", server, "OCP_TOKEN", false)
	sess := newSession(t)
	id := tools.HoldPasted(sess, tools.Pasted{Kind: "a login", SecretRef: "OCP_TOKEN", Server: server})
	err := login.PrecheckContext(context.Background(), sess, json.RawMessage(fmt.Sprintf(`{"cluster":"x","pasted":%q}`, id)))
	if err == nil || !strings.Contains(err.Error(), "already kept") {
		t.Fatalf("%v", err)
	}
}
