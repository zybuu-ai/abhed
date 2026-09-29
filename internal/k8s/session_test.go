package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// authLog is a fake cluster over TLS that accepts one token and notes every
// one it is sent. The CA file verifies it.
func authLog(t *testing.T, accept string) (srv *httptest.Server, ca string, seen func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv, ca = tlsCluster(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+accept {
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
	return srv, ca, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// One manager serves every session on a server. A login in one session must
// never be the credential another session's k8s_get uses.
func TestLoginIsScopedToTheSession(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kubeconfig func(server string) string
	}{
		// B has the operator's kubeconfig: it must keep using that.
		{"kubeconfig", func(server string) string { return writeKubeconfig(t, server) }},
		// B has nothing configured: it must fail, not borrow A's login.
		{"no kubeconfig", func(string) string { return t.TempDir() + "/missing" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ca, seen := authLog(t, "tok-A-only")
			mgr := NewManager(Config{Kubeconfig: tc.kubeconfig(srv.URL),
				Clusters: []LoginCluster{{Name: "prod", Server: srv.URL, CAFile: ca}}})
			a, b := newSession(t), newSession(t)

			login := LoginTool{M: mgr, Secret: stored(map[string]string{"OCP_TOKEN": "tok-A-only"})}
			args, _ := json.Marshal(map[string]string{"cluster": "prod", "token_secret": "OCP_TOKEN"})
			if res := login.Run(context.Background(), a, args); res.IsError {
				t.Fatalf("session A could not log in: %s", res.Content)
			}
			get, _ := json.Marshal(map[string]string{"resource": "nodes"})
			if res := (GetTool{M: mgr}).Run(context.Background(), a, get); res.IsError {
				t.Fatalf("session A cannot use its own login: %s", res.Content)
			}

			before := len(seen())
			res := GetTool{M: mgr}.Run(context.Background(), b, get)
			if !res.IsError {
				t.Fatalf("session B read the cluster with session A's login: %s", res.Content)
			}
			for _, auth := range seen()[before:] {
				if strings.Contains(auth, "tok-A-only") {
					t.Fatalf("session B's k8s_get sent session A's token")
				}
			}
			// Nor does a caller with no session at all.
			if res := (GetTool{M: mgr}).Run(context.Background(), nil, get); !res.IsError {
				t.Fatalf("a call with no session used session A's login: %s", res.Content)
			}
		})
	}
}

// Isolation cannot be guaranteed with no session to hold the login, so
// k8s_login refuses rather than keep it where every caller would find it.
func TestLoginRefusesWithoutASession(t *testing.T) {
	srv, ca, _ := authLog(t, "tok")
	mgr := NewManager(Config{Clusters: []LoginCluster{{Name: "c", Server: srv.URL, CAFile: ca}}})
	args, _ := json.Marshal(map[string]string{"cluster": "c", "token_secret": "T"})
	res := LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}.Run(context.Background(), nil, args)
	if !res.IsError {
		t.Fatalf("logged in with no session: %s", res.Content)
	}
}

// The token is not an argument: a call that sends one the old way has it
// dropped before policy, approval or the record see the arguments.
func TestLoginTakesNoTokenArgument(t *testing.T) {
	if strings.Contains(string(LoginTool{}.Schema()), `"token"`) {
		t.Fatal("k8s_login's schema still asks the model for the token")
	}
	canon, dropped, err := tools.CanonicalArgs(LoginTool{}, json.RawMessage(
		`{"cluster":"prod","token":"sha256~pasted","token_secret":"OCP_TOKEN"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canon), "sha256~pasted") {
		t.Fatalf("a token sent as an argument was kept: %s", canon)
	}
	if len(dropped) != 1 || dropped[0] != "token" {
		t.Fatalf("dropped = %v, want [token]", dropped)
	}
	// A token put where the name belongs is withheld the same way.
	got := tools.WithholdSecretValues(LoginTool{}, json.RawMessage(`{"cluster":"prod","token_secret":"sha256~pasted"}`))
	if strings.Contains(string(got), "sha256~pasted") {
		t.Fatalf("a token in token_secret was kept: %s", got)
	}
	if names := tools.SecretNames(LoginTool{}, json.RawMessage(`{"token_secret":"OCP_TOKEN"}`)); len(names) != 1 || names[0] != "OCP_TOKEN" {
		t.Fatalf("token_secret is not put to a secret rule: %v", names)
	}
}

// counted is a fake cluster over TLS that counts every request that reached it.
func counted(t *testing.T) (*httptest.Server, string, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv, ca := tlsCluster(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		fmt.Fprint(w, `{"major":"1","minor":"29"}`)
	}))
	return srv, ca, &n
}

// A login verifies the server's certificate. A self-signed cluster is refused
// until its CA is configured, per cluster or as k8s.ca_file, and the token is
// never sent to it: the handshake fails before any request.
func TestLoginVerifiesTLS(t *testing.T) {
	srv, ca, reached := counted(t)
	secret := stored(map[string]string{"T": "tok-tls-9c1"})
	args, _ := json.Marshal(map[string]string{"cluster": "lab", "token_secret": "T"})
	for _, tc := range []struct {
		name    string
		cfg     Config
		succeed bool
	}{
		{"no CA", Config{Clusters: []LoginCluster{{Name: "lab", Server: srv.URL}}}, false},
		{"cluster CA", Config{Clusters: []LoginCluster{{Name: "lab", Server: srv.URL, CAFile: ca}}}, true},
		{"k8s.ca_file", Config{CAFile: ca, Clusters: []LoginCluster{{Name: "lab", Server: srv.URL}}}, true},
		{"insecure flag", Config{Clusters: []LoginCluster{{Name: "lab", Server: srv.URL, InsecureSkipTLSVerify: true}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := reached.Load()
			res := LoginTool{M: NewManager(tc.cfg), Secret: secret}.Run(context.Background(), newSession(t), args)
			if res.IsError == tc.succeed {
				t.Fatalf("succeeded = %v, want %v: %s", !res.IsError, tc.succeed, res.Content)
			}
			if !tc.succeed && reached.Load() != before {
				t.Fatal("the token was sent to a server whose certificate did not verify")
			}
			if tc.name == "insecure flag" && !strings.Contains(res.Content, "NOT VERIFIED") {
				t.Errorf("an unverified login does not say so: %s", res.Content)
			}
		})
	}
}

// The model names a declared cluster; a URL, or a name the operator did not
// declare, is refused before the secret is read or any request is made.
func TestLoginRefusesAnUndeclaredServer(t *testing.T) {
	declared, ca, _ := counted(t)
	attacker, _, reached := counted(t)
	read := false
	secret := func(string) (string, error) { read = true; return "tok-undeclared-4b7", nil }
	login := LoginTool{M: NewManager(Config{CAFile: ca,
		Clusters: []LoginCluster{{Name: "prod", Server: declared.URL}}}), Secret: secret}

	for _, raw := range []string{
		`{"cluster":"` + attacker.URL + `","token_secret":"T"}`,
		`{"cluster":"staging","token_secret":"T"}`,
		`{"server":"` + attacker.URL + `","token_secret":"T"}`,
	} {
		canon, _, err := tools.CanonicalArgs(login, json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		if login.Precheck(nil, canon) == nil {
			t.Errorf("precheck let %s through to approval", raw)
		}
		if res := login.Run(context.Background(), newSession(t), canon); !res.IsError {
			t.Errorf("logged in with %s: %s", raw, res.Content)
		}
	}
	if reached.Load() != 0 {
		t.Fatalf("%d requests reached a server the operator did not declare", reached.Load())
	}
	if read {
		t.Fatal("the secret was read for a cluster that is not declared")
	}
	// With nothing declared, nothing is reachable.
	none := LoginTool{M: NewManager(Config{}), Secret: secret}
	if res := none.Run(context.Background(), newSession(t), json.RawMessage(`{"cluster":"prod","token_secret":"T"}`)); !res.IsError {
		t.Fatalf("logged in with no clusters declared: %s", res.Content)
	}
}

// The person approving, and the record, see where the token goes.
func TestLoginTargetNamesClusterAndServer(t *testing.T) {
	login := LoginTool{M: NewManager(Config{Clusters: []LoginCluster{
		{Name: "prod", Server: "https://api.prod.example:6443"},
		{Name: "lab", Server: "https://lab.example:6443", InsecureSkipTLSVerify: true}}})}
	got := login.Target(nil, json.RawMessage(`{"cluster":"prod","token_secret":"OCP_TOKEN"}`))
	for _, want := range []string{"OCP_TOKEN", "prod", "https://api.prod.example:6443", "system roots"} {
		if !strings.Contains(got, want) {
			t.Errorf("target %q does not name %s", got, want)
		}
	}
	if got := login.Target(nil, json.RawMessage(`{"cluster":"lab","token_secret":"T"}`)); !strings.Contains(got, "NOT VERIFIED") {
		t.Errorf("an insecure cluster's target does not warn: %q", got)
	}
}

// A kubeconfig context at the same server as a declared cluster may skip TLS
// verification and carries the operator's own credential. A login token is
// never put on that client: the prompt said the token goes over verified TLS.
func TestLoginTokenNeverRidesAKubeconfigClient(t *testing.T) {
	srv, ca, seen := authLog(t, "tok-login-7a3")
	kubeconfig := writeKubeconfig(t, srv.URL) // context "ctx", insecure-skip-tls-verify, token tok-123
	mgr := NewManager(Config{Kubeconfig: kubeconfig,
		Clusters: []LoginCluster{{Name: "prod", Server: srv.URL, CAFile: ca}}})
	sess := newSession(t)
	args, _ := json.Marshal(map[string]string{"cluster": "prod", "token_secret": "T"})
	if res := (LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok-login-7a3"})}).Run(context.Background(), sess, args); res.IsError {
		t.Fatalf("login failed: %s", res.Content)
	}

	before := len(seen())
	get, _ := json.Marshal(map[string]string{"resource": "nodes", "context": "ctx"})
	_ = GetTool{M: mgr}.Run(context.Background(), sess, get)
	for _, auth := range seen()[before:] {
		if strings.Contains(auth, "tok-login-7a3") {
			t.Fatal("the login token went out on the kubeconfig's unverified client")
		}
	}
	c, err := mgr.cluster(sess, "", "ctx")
	if err != nil {
		t.Fatal(err)
	}
	if c.bearer == "tok-login-7a3" {
		t.Fatal("the kubeconfig client carries the login token")
	}

	// Named as the declared cluster, the login is used, verified.
	byName, _ := json.Marshal(map[string]string{"resource": "nodes", "cluster": "prod"})
	if res := (GetTool{M: mgr}).Run(context.Background(), sess, byName); res.IsError {
		t.Fatalf("the login is not usable by its cluster's name: %s", res.Content)
	}
}

// Two logins in one session are each reachable by name; with neither named,
// the call is refused rather than guessed.
func TestTwoLoginsAreChosenByName(t *testing.T) {
	a, caA, seenA := authLog(t, "tok-a")
	b, caB, seenB := authLog(t, "tok-b")
	mgr := NewManager(Config{Kubeconfig: t.TempDir() + "/missing", Clusters: []LoginCluster{
		{Name: "a", Server: a.URL, CAFile: caA}, {Name: "b", Server: b.URL, CAFile: caB}}})
	sess := newSession(t)
	login := LoginTool{M: mgr, Secret: stored(map[string]string{"TA": "tok-a", "TB": "tok-b"})}
	for _, l := range []map[string]string{{"cluster": "a", "token_secret": "TA"}, {"cluster": "b", "token_secret": "TB"}} {
		raw, _ := json.Marshal(l)
		if res := login.Run(context.Background(), sess, raw); res.IsError {
			t.Fatalf("login %v failed: %s", l, res.Content)
		}
	}
	for _, name := range []string{"a", "b"} {
		raw, _ := json.Marshal(map[string]string{"resource": "nodes", "cluster": name})
		if res := (GetTool{M: mgr}).Run(context.Background(), sess, raw); res.IsError {
			t.Fatalf("cluster %s: %s", name, res.Content)
		}
	}
	if la, lb := seenA(), seenB(); la[len(la)-1] != "Bearer tok-a" || lb[len(lb)-1] != "Bearer tok-b" {
		t.Fatalf("a login went to the other cluster: %v %v", la, lb)
	}
	raw, _ := json.Marshal(map[string]string{"resource": "nodes"})
	if res := (GetTool{M: mgr}).Run(context.Background(), sess, raw); !res.IsError || !strings.Contains(res.Content, "more than one cluster") {
		t.Fatalf("an unnamed call with two logins was not refused: %s", res.Content)
	}
	// A cluster the session has not logged in to is refused.
	other := newSession(t)
	byName, _ := json.Marshal(map[string]string{"resource": "nodes", "cluster": "a"})
	if res := (GetTool{M: mgr}).Run(context.Background(), other, byName); !res.IsError {
		t.Fatalf("another session used cluster a without logging in: %s", res.Content)
	}
}

// When the session goes, its logins go with it: the credential is forgotten
// and its connections are closed, not left open until the process ends.
func TestClosingTheSessionReleasesItsLogins(t *testing.T) {
	var open atomic.Int32
	srv, ca := tlsCluster(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/version") {
			fmt.Fprint(w, `{"major":"1","minor":"29"}`)
			return
		}
		fmt.Fprint(w, `{"kind":"NodeList","items":[]}`)
	}), func(hs *http.Server) {
		hs.ConnState = func(_ net.Conn, st http.ConnState) {
			switch st {
			case http.StateNew:
				open.Add(1)
			case http.StateClosed, http.StateHijacked:
				open.Add(-1)
			}
		}
	})
	mgr := NewManager(Config{Clusters: []LoginCluster{{Name: "c", Server: srv.URL, CAFile: ca}}})
	sess := newSession(t)
	args, _ := json.Marshal(map[string]string{"cluster": "c", "token_secret": "T"})
	login := LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}
	if res := login.Run(context.Background(), sess, args); res.IsError {
		t.Fatalf("login failed: %s", res.Content)
	}
	get, _ := json.Marshal(map[string]string{"resource": "nodes", "cluster": "c"})
	if res := (GetTool{M: mgr}).Run(context.Background(), sess, get); res.IsError {
		t.Fatal(res.Content)
	}
	// Logging in again replaces the client and closes the old one's connections.
	if res := login.Run(context.Background(), sess, args); res.IsError {
		t.Fatal(res.Content)
	}
	waitFor(t, func() bool { return open.Load() == 0 }, "a replaced login's connections stayed open")

	if res := (GetTool{M: mgr}).Run(context.Background(), sess, get); res.IsError {
		t.Fatal(res.Content)
	}
	sess.CloseScoped()
	waitFor(t, func() bool { return open.Load() == 0 }, "the session's connections stayed open after it ended")
	if res := (GetTool{M: mgr}).Run(context.Background(), sess, get); !res.IsError {
		t.Fatalf("the login outlived its session: %s", res.Content)
	}
}

func waitFor(t *testing.T, ok func() bool, why string) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatal(why)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
