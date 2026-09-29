package k8s

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// fakeAPIServer stands in for a cluster, recording what was asked of it.
func fakeAPIServer(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Method+" "+r.URL.RequestURI()+" auth="+r.Header.Get("Authorization"))
		switch {
		case strings.HasSuffix(r.URL.Path, "/pods"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"kind":"PodList","items":[
			  {"metadata":{"name":"api-0","namespace":"prod"},
			   "status":{"phase":"Running","containerStatuses":[{"ready":true,"restartCount":0}]}}]}`)
		case strings.Contains(r.URL.Path, "/log"):
			_, _ = fmt.Fprint(w, "line one\nline two\n")
		case r.Method == http.MethodDelete:
			_, _ = fmt.Fprint(w, `{"kind":"Status","status":"Success"}`)
		case r.Method == http.MethodPatch:
			_, _ = fmt.Fprintf(w, `{"kind":"Deployment","metadata":{"name":"web"},"contentType":%q}`,
				r.Header.Get("Content-Type"))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"kind":"Status","message":"the server could not find the requested resource"}`)
		}
	}))
}

// writeKubeconfig points a config at the fake server.
func writeKubeconfig(t *testing.T, server string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	body := fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
    server: %s
    insecure-skip-tls-verify: true
  name: c
contexts:
- context:
    cluster: c
    user: u
    namespace: prod
  name: ctx
current-context: ctx
users:
- name: u
  user:
    token: tok-123
`, server)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGetPodsAgainstFakeCluster(t *testing.T) {
	var seen []string
	srv := fakeAPIServer(t, &seen)
	defer srv.Close()

	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
	args, _ := json.Marshal(map[string]string{"resource": "pods"})
	res := GetTool{M: mgr}.Run(context.Background(), nil, args)

	if res.IsError {
		t.Fatalf("get pods failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "prod/api-0") || !strings.Contains(res.Content, "Running") {
		t.Errorf("unexpected rendering: %s", res.Content)
	}
	// The namespace must come from the context, and the token must be sent.
	if len(seen) != 1 || !strings.Contains(seen[0], "/api/v1/namespaces/prod/pods") {
		t.Errorf("wrong request: %v", seen)
	}
	if !strings.Contains(seen[0], "auth=Bearer tok-123") {
		t.Errorf("bearer token not sent: %v", seen)
	}
}

func TestGetLogs(t *testing.T) {
	var seen []string
	srv := fakeAPIServer(t, &seen)
	defer srv.Close()

	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
	args, _ := json.Marshal(map[string]any{
		"resource": "logs", "name": "api-0", "tail": 50, "container": "app"})
	res := GetTool{M: mgr}.Run(context.Background(), nil, args)

	if res.IsError || !strings.Contains(res.Content, "line one") {
		t.Fatalf("logs: %s", res.Content)
	}
	if !strings.Contains(seen[0], "tailLines=50") || !strings.Contains(seen[0], "container=app") {
		t.Errorf("log parameters not passed: %v", seen)
	}
}

// A 404 from the API must arrive as the server's own message, which says
// which resource was missing.
func TestNotFoundCarriesTheAPIMessage(t *testing.T) {
	var seen []string
	srv := fakeAPIServer(t, &seen)
	defer srv.Close()

	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
	args, _ := json.Marshal(map[string]string{"resource": "services"})
	res := GetTool{M: mgr}.Run(context.Background(), nil, args)

	if !res.IsError {
		t.Fatal("a 404 was reported as success")
	}
	if !strings.Contains(res.Content, "could not find") {
		t.Errorf("API message lost: %s", res.Content)
	}
}

// RBAC denials must be distinguishable from every other failure, because the
// fix is different: ask for permission, not retry.
func TestForbiddenIsNamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"kind":"Status","message":"pods is forbidden: User cannot list resource"}`)
	}))
	defer srv.Close()

	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
	args, _ := json.Marshal(map[string]string{"resource": "pods"})
	res := GetTool{M: mgr}.Run(context.Background(), nil, args)

	if !strings.Contains(res.Content, "RBAC") || !strings.Contains(res.Content, "cannot list") {
		t.Errorf("forbidden not surfaced usefully: %s", res.Content)
	}
}

func TestScaleSendsMergePatch(t *testing.T) {
	var seen []string
	srv := fakeAPIServer(t, &seen)
	defer srv.Close()

	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
	args, _ := json.Marshal(map[string]any{
		"action": "scale", "resource": "deployments", "name": "web", "replicas": 3})
	res := ApplyTool{M: mgr}.Run(context.Background(), nil, args)

	if res.IsError {
		t.Fatalf("scale: %s", res.Content)
	}
	if !strings.Contains(seen[0], "PATCH") || !strings.Contains(seen[0], "/deployments/web/scale") {
		t.Errorf("wrong scale request: %v", seen)
	}
	if !strings.Contains(res.Content, "3 replicas") {
		t.Errorf("unclear confirmation: %s", res.Content)
	}
}

func TestDeleteHitsTheRightPath(t *testing.T) {
	var seen []string
	srv := fakeAPIServer(t, &seen)
	defer srv.Close()

	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL)})
	args, _ := json.Marshal(map[string]string{
		"action": "delete", "resource": "pods", "name": "api-0"})
	res := ApplyTool{M: mgr}.Run(context.Background(), nil, args)

	if res.IsError {
		t.Fatalf("delete: %s", res.Content)
	}
	if !strings.HasPrefix(seen[0], "DELETE /api/v1/namespaces/prod/pods/api-0") {
		t.Errorf("wrong delete request: %v", seen)
	}
}

// Naming a context that does not exist must list the ones that do.
func TestUnknownContextListsAvailable(t *testing.T) {
	path := writeKubeconfig(t, "https://unused")
	_, err := Open(Config{Kubeconfig: path, Context: "nope"})
	if err == nil {
		t.Fatal("accepted an unknown context")
	}
	if !strings.Contains(err.Error(), "ctx") {
		t.Errorf("error does not list available contexts: %v", err)
	}
}

// stored stands in for the secrets store.
func stored(vals map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		v, ok := vals[name]
		if !ok {
			return "", fmt.Errorf("no secret named %s is stored", name)
		}
		return v, nil
	}
}

// tlsCluster starts a fake cluster over TLS with its own self-signed
// certificate, and returns the CA file that verifies it.
func tlsCluster(t *testing.T, h http.Handler, configure ...func(*http.Server)) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	for _, f := range configure {
		f(srv.Config)
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, ca
}

func newSession(t *testing.T) *tools.Session {
	t.Helper()
	s, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A credential supplied at runtime must override the kubeconfig entirely. The
// reported failure was a stale token in ~/.kube/config producing a 401 while
// the user had a working token in hand and no way to hand it over.
func TestLoginOverridesStaleKubeconfig(t *testing.T) {
	var authSeen []string
	srv, ca := tlsCluster(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		authSeen = append(authSeen, auth)
		if auth != "Bearer fresh-token" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"kind":"Status","message":"Unauthorized"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/version") {
			fmt.Fprint(w, `{"major":"1","minor":"29"}`)
			return
		}
		fmt.Fprint(w, `{"kind":"NodeList","items":[{"metadata":{"name":"n1"}}]}`)
	}))

	// The kubeconfig holds a stale token, as in the real report.
	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, srv.URL),
		Clusters: []LoginCluster{{Name: "prod", Server: srv.URL, CAFile: ca}}})
	sess := newSession(t)

	// Before login: the stale token fails with a message naming the fix.
	args, _ := json.Marshal(map[string]string{"resource": "nodes"})
	res := GetTool{M: mgr}.Run(context.Background(), sess, args)
	if !res.IsError {
		t.Fatal("a stale token was accepted")
	}
	if !strings.Contains(res.Content, "expired") || !strings.Contains(res.Content, "ABHED_K8S_TOKEN") {
		t.Errorf("401 does not name the fix: %s", res.Content)
	}

	// Login with a working token, named in the store.
	login := LoginTool{M: mgr, Secret: stored(map[string]string{"OCP_TOKEN": "fresh-token"})}
	loginArgs, _ := json.Marshal(map[string]string{"cluster": "prod", "token_secret": "OCP_TOKEN"})
	lres := login.Run(context.Background(), sess, loginArgs)
	if lres.IsError {
		t.Fatalf("login failed: %s", lres.Content)
	}
	if !strings.Contains(lres.Content, "not written to your kubeconfig") {
		t.Errorf("login does not say where the credential lives: %s", lres.Content)
	}

	// After login: the same call succeeds, using the new credential.
	res = GetTool{M: mgr}.Run(context.Background(), sess, args)
	if res.IsError {
		t.Fatalf("still failing after login: %s", res.Content)
	}
	if !strings.Contains(res.Content, "n1") {
		t.Errorf("unexpected result: %s", res.Content)
	}
	if authSeen[len(authSeen)-1] != "Bearer fresh-token" {
		t.Errorf("the cached client kept the old token: %v", authSeen)
	}
}

// Storing a credential that does not work would turn one clear failure into a
// confusing one on the next call.
func TestLoginVerifiesBeforeStoring(t *testing.T) {
	srv, ca := tlsCluster(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"kind":"Status","message":"Unauthorized"}`)
	}))

	mgr := NewManager(Config{Clusters: []LoginCluster{{Name: "c", Server: srv.URL, CAFile: ca}}})
	sess := newSession(t)
	args, _ := json.Marshal(map[string]string{"cluster": "c", "token_secret": "BAD"})
	res := LoginTool{M: mgr, Secret: stored(map[string]string{"BAD": "bad"})}.Run(context.Background(), sess, args)
	if !res.IsError {
		t.Fatal("a token that does not work was accepted")
	}
	if mgr.logins(sess, false) != nil {
		t.Error("a failing credential was stored anyway")
	}
}

// Login changes which cluster the agent can reach and as whom, so it needs the
// same confirmation as a write.
func TestLoginRequiresApproval(t *testing.T) {
	if !(LoginTool{}).Mutates() {
		t.Error("k8s_login does not declare itself mutating, so it could run unapproved")
	}
}

func TestLoginValidatesArguments(t *testing.T) {
	mgr := NewManager(Config{Clusters: []LoginCluster{
		{Name: "x", Server: "https://x.invalid"}, {Name: "plain", Server: "http://x.invalid"}}})
	login := LoginTool{M: mgr, Secret: stored(map[string]string{"TOK": "y"})}
	for _, a := range []map[string]string{
		{"token_secret": "TOK"},                        // no cluster
		{"cluster": "x"},                               // no secret
		{"cluster": "plain", "token_secret": "TOK"},    // not https
		{"cluster": "x", "token_secret": "sha256~abc"}, // a token, not a name
		{"cluster": "x", "token_secret": "MISSING"},    // not stored
	} {
		raw, _ := json.Marshal(a)
		if res := login.Run(context.Background(), newSession(t), raw); !res.IsError {
			t.Errorf("accepted invalid login args %v", a)
		}
	}
}
