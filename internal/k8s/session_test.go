package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
			if res := (GetTool{M: mgr}).Run(context.Background(), a, resolved(t, GetTool{M: mgr}, a, get)); res.IsError {
				t.Fatalf("session A cannot use its own login: %s", res.Content)
			}

			before := len(seen())
			res := GetTool{M: mgr}.Run(context.Background(), b, resolved(t, GetTool{M: mgr}, b, get))
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

// The approval for a write says which cluster it changes, at which server,
// and with whose credential.
func TestApplyTargetNamesClusterServerAndCredential(t *testing.T) {
	srv, ca, _ := authLog(t, "tok")
	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, "https://kube.example:6443"),
		Clusters: []LoginCluster{{Name: "prod", Server: srv.URL, CAFile: ca}}})
	apply := ApplyTool{M: mgr}
	sess := newSession(t)

	if got := apply.Target(sess, json.RawMessage(`{"action":"delete","context":"ctx"}`)); !strings.Contains(got, "context ctx at https://kube.example:6443") ||
		!strings.Contains(got, "kubeconfig's own credential") {
		t.Errorf("a kubeconfig write does not say where and with what: %q", got)
	}
	if got := apply.Target(sess, json.RawMessage(`{"action":"delete","cluster":"prod"}`)); !strings.Contains(got, "not logged in") {
		t.Errorf("a write to a cluster not logged in to does not say so: %q", got)
	}
	args, _ := json.Marshal(map[string]string{"cluster": "prod", "token_secret": "T"})
	if res := (LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}).Run(context.Background(), sess, args); res.IsError {
		t.Fatal(res.Content)
	}
	for _, raw := range []string{`{"action":"delete","cluster":"prod"}`, `{"action":"delete"}`} {
		got := apply.Target(sess, resolved(t, apply, sess, json.RawMessage(raw)))
		for _, want := range []string{"cluster prod at " + srv.URL, "this session's login", "TLS verified"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: target %q does not name %q", raw, got, want)
			}
		}
	}
}

// A user or password written into a kubeconfig or declared server is not
// shown for approval or recorded.
func TestApplyTargetLeavesOutUserinfo(t *testing.T) {
	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, "https://admin:kube-pw-3e1@kube.example:6443"),
		Clusters: []LoginCluster{{Name: "prod", Server: "https://svc:decl-pw-8a0@api.prod.example"}}})
	for _, raw := range []string{`{"action":"delete","context":"ctx"}`, `{"action":"delete","cluster":"prod"}`} {
		got := ApplyTool{M: mgr}.Target(newSession(t), json.RawMessage(raw))
		if got == "" {
			t.Fatalf("%s: no target", raw)
		}
		for _, secret := range []string{"kube-pw-3e1", "decl-pw-8a0", "admin", "svc:"} {
			if strings.Contains(got, secret) {
				t.Errorf("%s: target %q repeats %s", raw, got, secret)
			}
		}
	}
	got := LoginTool{M: mgr}.Target(nil, json.RawMessage(`{"cluster":"prod","token_secret":"T"}`))
	if strings.Contains(got, "decl-pw-8a0") {
		t.Errorf("the login target repeats the server's password: %q", got)
	}
}

// What was approved is what runs: a kubeconfig edited between the approval
// and the call does not send the write to another server than the one named.
func TestApplyGoesWhereItsApprovalSaid(t *testing.T) {
	var hitA, hitB atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitA.Add(1)
		fmt.Fprint(w, `{"kind":"Status","status":"Success"}`)
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitB.Add(1)
		fmt.Fprint(w, `{"kind":"Status","status":"Success"}`)
	}))
	defer b.Close()
	kube := writeKubeconfig(t, a.URL)
	mgr := NewManager(Config{Kubeconfig: kube})
	apply := ApplyTool{M: mgr}
	sess := newSession(t)
	args := json.RawMessage(`{"action":"delete","resource":"pods","name":"web","context":"ctx"}`)

	target := apply.Target(sess, args)
	if !strings.Contains(target, a.URL) {
		t.Fatalf("target %q does not name the server", target)
	}
	moved, err := os.ReadFile(writeKubeconfig(t, b.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kube, moved, 0o600); err != nil {
		t.Fatal(err)
	}
	if res := apply.Run(context.Background(), sess, args); res.IsError {
		t.Fatalf("the approved write failed: %s", res.Content)
	}
	if hitB.Load() != 0 || hitA.Load() != 1 {
		t.Fatalf("approved %q, but the write reached A %d times and B %d times", target, hitA.Load(), hitB.Load())
	}
}

// A token in a server's query or fragment is left out too, and a kubeconfig
// that cannot be opened is named with a reason that holds none of its values.
func TestApplyTargetLeavesOutQueryAndExplainsFailure(t *testing.T) {
	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, "https://kube.example:6443/?access_token=q-tok-11#frag-tok-22")})
	got := ApplyTool{M: mgr}.Target(newSession(t), json.RawMessage(`{"action":"delete","context":"ctx"}`))
	if !strings.Contains(got, "https://kube.example:6443") || strings.Contains(got, "q-tok-11") || strings.Contains(got, "frag-tok-22") {
		t.Fatalf("target %q", got)
	}

	bad := filepath.Join(t.TempDir(), "config")
	body := "apiVersion: v1\nusers:\n- name: u\n  user:\n    token: bad-kube-tok-33\n   oops\n"
	if err := os.WriteFile(bad, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got = ApplyTool{M: NewManager(Config{Kubeconfig: bad})}.Target(newSession(t), json.RawMessage(`{"action":"delete","context":"ctx"}`))
	// The model's context name is quoted, so it cannot read as Abhed's words.
	if !strings.Contains(got, `context "ctx" could not be opened`) || strings.Contains(got, "bad-kube-tok-33") {
		t.Fatalf("target for a broken kubeconfig: %q", got)
	}
}

// Parallel reads share one kubeconfig client. On first use they must run the
// exec helper once, under the lock, and each send the token it produced; run
// with -race, this also catches an unguarded bearer.
func TestExecCredentialFirstUseIsSerialized(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	helper := filepath.Join(dir, "helper.sh")
	script := "#!/bin/sh\necho ran >> '" + marker + "'\nsleep 0.2\n" +
		`echo '{"status":{"token":"exec-tok-6d4"}}'` + "\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var bad atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer exec-tok-6d4" {
			bad.Add(1)
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c := &Cluster{Server: srv.URL, execCfg: &execConfig{Command: helper}, client: &http.Client{}}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Do(context.Background(), "GET", "/version", nil); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	data, _ := os.ReadFile(marker)
	if n := strings.Count(string(data), "ran"); n != 1 {
		t.Fatalf("the helper ran %d times for eight first requests, want once", n)
	}
	if bad.Load() != 0 {
		t.Fatalf("%d requests went without the helper's token", bad.Load())
	}
}

// A request that cannot reach the cluster is reported without the request
// URL, whose query may hold a token the kubeconfig put in the server.
func TestUnreachableErrorCarriesNoQuery(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens there now
	c := &Cluster{Server: "http://" + addr + "/?access_token=q-net-44", client: &http.Client{Timeout: 5 * time.Second}}
	for _, call := range []func() error{
		func() error { _, err := c.Do(context.Background(), "GET", "/version", nil); return err },
		func() error {
			_, err := c.doPatch(context.Background(), "/x", []byte(`{}`), "application/merge-patch+json")
			return err
		},
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "cannot reach the cluster at http://"+addr) {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), "q-net-44") {
			t.Fatalf("the error repeats the server's query: %v", err)
		}
	}
}

// resolved is what the loop passes on: the arguments with the session's only
// login named when the call names no cluster or context.
func resolved(t *testing.T, r tools.ArgResolver, sess *tools.Session, raw json.RawMessage) json.RawMessage {
	t.Helper()
	if out, _ := r.ResolveArgs(sess, raw); out != nil {
		return out
	}
	return raw
}
