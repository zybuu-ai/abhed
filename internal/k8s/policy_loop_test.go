package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// oneCall asks for one tool call, then ends the run.
type oneCall struct {
	name string
	args string
	n    int
}

func (*oneCall) Name() string                           { return "one-call" }
func (*oneCall) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (*oneCall) CountTokens(model.Request) (int, error) { return 0, nil }
func (o *oneCall) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 2)
	if o.n == 0 {
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &model.ToolCall{ID: "c1", Name: o.name, Args: json.RawMessage(o.args)}}
	} else {
		ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	}
	o.n++
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
	close(ch)
	return ch, nil
}

// asked keeps what the approver was shown and approves it.
type asked struct {
	mu     sync.Mutex
	scopes []string
	args   []string
}

func (a *asked) Approve(_ context.Context, _ string, args json.RawMessage, res policy.Result) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.scopes, a.args = append(a.scopes, res.Offer()), append(a.args, string(args))
	return true, nil
}

// A call that names no cluster goes to the session's only login, so the loop
// judges it as a call to that cluster: a deny rule on the cluster holds, and
// the scope offered names it.
func TestUnnamedCallIsJudgedAsTheLoginsCluster(t *testing.T) {
	srv, ca, seen := authLog(t, "tok")
	mgr := NewManager(Config{Kubeconfig: t.TempDir() + "/missing",
		Clusters: []LoginCluster{{Name: "prod", Server: srv.URL, CAFile: ca}}})
	reg := tools.NewRegistry(GetTool{M: mgr}, ApplyTool{M: mgr})
	run := func(pol *policy.Engine, sess *tools.Session, name, args string) (*asked, []agent.Event) {
		t.Helper()
		ap := &asked{}
		store := agent.NewMemStore()
		l := agent.NewLoop(&oneCall{name: name, args: args}, reg, pol, ap, sess,
			agent.NewRecorder(store, "s", ""), agent.DefaultConfig())
		if _, err := l.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		evs, _ := store.Events("s")
		return ap, evs
	}
	sess := newSession(t)
	login, _ := json.Marshal(map[string]string{"cluster": "prod", "token_secret": "T"})
	if res := (LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}).Run(context.Background(), sess, login); res.IsError {
		t.Fatal(res.Content)
	}

	for _, mode := range []policy.Mode{policy.ModeDefault, policy.ModeAuto, policy.ModeBypass} {
		pol := policy.New(mode)
		if err := pol.AddDeny("k8s_get(prod/*)"); err != nil {
			t.Fatal(err)
		}
		before := len(seen())
		_, evs := run(pol, sess, "k8s_get", `{"resource":"nodes"}`)
		denied := false
		for _, ev := range evs {
			if ev.Type == agent.EvActionDenied && strings.Contains(string(ev.Payload), "k8s_get(prod/*)") {
				denied = true
			}
		}
		if !denied || len(seen()) != before {
			t.Fatalf("%s: an unnamed k8s_get reached prod past deny k8s_get(prod/*)", mode)
		}
	}

	ap, _ := run(policy.New(policy.ModeDefault), sess, "k8s_apply", `{"action":"restart","resource":"deployments","name":"web"}`)
	if len(ap.scopes) != 1 || ap.scopes[0] != "k8s_apply(prod/default/restart)" || !strings.Contains(ap.args[0], `"cluster":"prod"`) {
		t.Fatalf("the unnamed write was not put to the approver as a write to prod: %v %v", ap.scopes, ap.args)
	}
}

// A call judged as going to the kubeconfig does not reach a login made after
// it was judged.
func TestUnnamedCallDoesNotMeetALaterLogin(t *testing.T) {
	srv, ca, seen := authLog(t, "tok")
	mgr := NewManager(Config{Kubeconfig: t.TempDir() + "/missing",
		Clusters: []LoginCluster{{Name: "prod", Server: srv.URL, CAFile: ca}}})
	sess := newSession(t)
	get := json.RawMessage(`{"resource":"nodes"}`)
	if out, _, _ := (GetTool{M: mgr}).ResolveArgs(sess, get); out != nil {
		t.Fatalf("a session with no login named a cluster: %s", out)
	}
	login, _ := json.Marshal(map[string]string{"cluster": "prod", "token_secret": "T"})
	if res := (LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}).Run(context.Background(), sess, login); res.IsError {
		t.Fatal(res.Content)
	}
	before := len(seen())
	if res := (GetTool{M: mgr}).Run(context.Background(), sess, get); !res.IsError || len(seen()) != before {
		t.Fatalf("a call judged before the login used it: %s", res.Content)
	}
}

// runOne puts one call through a loop with pol and an approver that approves,
// and reports whether it was denied, and the record.
func runOne(t *testing.T, reg *tools.Registry, pol *policy.Engine, sess *tools.Session, name, args string) (bool, []agent.Event) {
	t.Helper()
	ap := &asked{}
	store := agent.NewMemStore()
	l := agent.NewLoop(&oneCall{name: name, args: args}, reg, pol, ap, sess,
		agent.NewRecorder(store, "s", ""), agent.DefaultConfig())
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("s")
	denied := false
	for _, ev := range evs {
		if ev.Type == agent.EvActionDenied && strings.Contains(string(ev.Payload), `"step":"deny"`) {
			denied = true
		}
	}
	return denied, evs
}

// A deny on a namespace's secrets holds however the call spells them: a
// singular, another case, every namespace, or no namespace when the login's
// default is that one; in every mode, bypass included. So does a rule on the
// resource alone, written as before.
func TestNamespaceDenyHoldsHoweverTheCallIsWritten(t *testing.T) {
	srv, ca, seen := authLog(t, "tok")
	mgr := NewManager(Config{Kubeconfig: t.TempDir() + "/missing",
		Clusters: []LoginCluster{{Name: "prod", Server: srv.URL, CAFile: ca}}})
	reg := tools.NewRegistry(GetTool{M: mgr}, ApplyTool{M: mgr})
	sess := newSession(t)
	login, _ := json.Marshal(map[string]string{"cluster": "prod", "token_secret": "T", "namespace": "kube-system"})
	if res := (LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}).Run(context.Background(), sess, login); res.IsError {
		t.Fatal(res.Content)
	}
	for _, rule := range []string{"k8s_get(*/kube-system/secrets)", "k8s_get(secrets*)"} {
		for _, mode := range []policy.Mode{policy.ModeDefault, policy.ModeAuto, policy.ModeBypass} {
			for _, args := range []string{
				`{"cluster":"prod","namespace":"kube-system","resource":"secrets"}`,
				`{"cluster":"prod","namespace":"kube-system","resource":"secret"}`,
				`{"cluster":"prod","namespace":"kube-system","resource":" Secrets"}`,
				`{"cluster":"prod","namespace":"*","resource":"secrets"}`,
				`{"cluster":"prod","resource":"secrets"}`,
				`{"resource":"Secret"}`,
			} {
				pol := policy.New(mode)
				if err := pol.AddDeny(rule); err != nil {
					t.Fatal(err)
				}
				before := len(seen())
				if denied, _ := runOne(t, reg, pol, sess, "k8s_get", args); !denied || len(seen()) != before {
					t.Errorf("%s, %s: %s was not denied", rule, mode, args)
				}
			}
		}
	}
	// Another namespace's secrets are not caught by the namespace rule.
	pol := policy.New(policy.ModeBypass)
	_ = pol.AddDeny("k8s_get(*/kube-system/secrets)")
	if denied, _ := runOne(t, reg, pol, sess, "k8s_get", `{"cluster":"prod","namespace":"web","resource":"secrets"}`); denied {
		t.Error("a rule on kube-system denied another namespace")
	}
}

// An apply is judged in the namespace its manifest names when the call names
// none, and the record says which arguments the harness filled in.
func TestApplyIsJudgedInTheManifestsNamespace(t *testing.T) {
	srv, ca, _ := authLog(t, "tok")
	mgr := NewManager(Config{Kubeconfig: t.TempDir() + "/missing",
		Clusters: []LoginCluster{{Name: "prod", Server: srv.URL, CAFile: ca}}})
	reg := tools.NewRegistry(GetTool{M: mgr}, ApplyTool{M: mgr})
	sess := newSession(t)
	login, _ := json.Marshal(map[string]string{"cluster": "prod", "token_secret": "T"})
	if res := (LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}).Run(context.Background(), sess, login); res.IsError {
		t.Fatal(res.Content)
	}
	manifest := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x","namespace":"kube-system"}}`
	args, _ := json.Marshal(map[string]string{"action": "apply", "manifest": manifest})
	for _, mode := range []policy.Mode{policy.ModeDefault, policy.ModeBypass} {
		pol := policy.New(mode)
		_ = pol.AddDeny("k8s_apply(prod/kube-system/*)")
		denied, evs := runOne(t, reg, pol, sess, "k8s_apply", string(args))
		if !denied {
			t.Fatalf("%s: an apply into kube-system, named only in its manifest, was not denied", mode)
		}
		var req agent.ActionRequested
		for _, ev := range evs {
			if ev.Type == agent.EvActionRequested {
				_ = json.Unmarshal(ev.Payload, &req)
			}
		}
		if strings.Join(req.Resolved, ",") != "cluster,namespace" || !strings.Contains(string(req.Args), `"namespace":"kube-system"`) {
			t.Fatalf("%s: the record does not say what the harness filled in: %v %s", mode, req.Resolved, req.Args)
		}
	}
}

// A call that names a kubeconfig context is not given the login's cluster.
func TestResolverLeavesAContextAlone(t *testing.T) {
	srv, ca, _ := authLog(t, "tok")
	mgr := NewManager(Config{Kubeconfig: writeKubeconfig(t, "https://kube.example:6443"),
		Clusters: []LoginCluster{{Name: "prod", Server: srv.URL, CAFile: ca}}})
	sess := newSession(t)
	login, _ := json.Marshal(map[string]string{"cluster": "prod", "token_secret": "T"})
	if res := (LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}).Run(context.Background(), sess, login); res.IsError {
		t.Fatal(res.Content)
	}
	out, which, _ := (GetTool{M: mgr}).ResolveArgs(sess, json.RawMessage(`{"resource":"pods","context":"ctx"}`))
	if strings.Contains(string(out), `"cluster"`) || strings.Contains(strings.Join(which, ","), "cluster") {
		t.Fatalf("a call naming a context was given the login's cluster: %s %v", out, which)
	}
}

// An apply takes one object, so a list whose items span namespaces is never
// judged by the namespace of the first.
func TestApplyRefusesAList(t *testing.T) {
	srv, ca, seen := authLog(t, "tok")
	mgr := NewManager(Config{Kubeconfig: t.TempDir() + "/missing",
		Clusters: []LoginCluster{{Name: "prod", Server: srv.URL, CAFile: ca}}})
	sess := newSession(t)
	login, _ := json.Marshal(map[string]string{"cluster": "prod", "token_secret": "T"})
	if res := (LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}).Run(context.Background(), sess, login); res.IsError {
		t.Fatal(res.Content)
	}
	for _, m := range []string{
		`{"apiVersion":"v1","kind":"List","metadata":{"name":"l"},"items":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a","namespace":"kube-system"}}]}`,
		`{"apiVersion":"v1","kind":"ConfigMapList","metadata":{"name":"l","namespace":"web"}}`,
	} {
		args, _ := json.Marshal(map[string]string{"cluster": "prod", "action": "apply", "manifest": m})
		before := len(seen())
		if res := (ApplyTool{M: mgr}).Run(context.Background(), sess, args); !res.IsError || len(seen()) != before {
			t.Errorf("a list was applied: %s", res.Content)
		}
	}
}

// pathLog is a fake cluster over TLS that accepts any token and keeps each
// request line as sent.
func pathLog(t *testing.T) (*httptest.Server, string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv, ca := tlsCluster(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.RequestURI)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"kind":"List","items":[]}`)
	}))
	return srv, ca, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
}

// A namespace, name, kind or apiVersion that could not stand as one path
// segment is refused before any rule reads the call and before any request,
// in bypass too; a selector is sent query-encoded.
func TestPathSegmentsCannotReachAnotherResource(t *testing.T) {
	srv, ca, sent := pathLog(t)
	mgr := NewManager(Config{Kubeconfig: t.TempDir() + "/missing",
		Clusters: []LoginCluster{{Name: "lab", Server: srv.URL, CAFile: ca}}})
	reg := tools.NewRegistry(GetTool{M: mgr}, ApplyTool{M: mgr})
	sess := newSession(t)
	login, _ := json.Marshal(map[string]string{"cluster": "lab", "token_secret": "T"})
	if res := (LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}).Run(context.Background(), sess, login); res.IsError {
		t.Fatal(res.Content)
	}
	pol := policy.New(policy.ModeBypass)
	if err := pol.AddDeny("k8s_get(*/kube-system/secrets)", "k8s_get(secrets*)"); err != nil {
		t.Fatal(err)
	}
	mani := func(apiVersion, kind, name, ns string) string {
		m, _ := json.Marshal(map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]string{"name": name, "namespace": ns}})
		a, _ := json.Marshal(map[string]string{"cluster": "lab", "action": "apply", "manifest": string(m)})
		return string(a)
	}
	for _, c := range []struct{ tool, args string }{
		{"k8s_get", `{"cluster":"lab","namespace":"kube-system/secrets?","resource":"pods"}`},
		{"k8s_get", `{"cluster":"lab","namespace":"kube-system","resource":"pods","name":"../secrets"}`},
		{"k8s_get", `{"cluster":"lab","namespace":"kube-system","resource":"pods","name":"%2e%2e"}`},
		{"k8s_get", `{"cluster":"lab","namespace":"kube-system","resource":"pods","name":"a#b"}`},
		{"k8s_get", `{"cluster":"lab","namespace":"kube-system","resource":"pods","name":"a\nb"}`},
		{"k8s_get", `{"cluster":"lab","namespace":"Kube System","resource":"pods"}`},
		{"k8s_get", `{"cluster":"lab","namespace":"kube-system","resource":"logs","name":"x/../../secrets"}`},
		{"k8s_apply", `{"cluster":"lab","namespace":"kube-system","action":"delete","resource":"pods","name":"../secrets/x"}`},
		{"k8s_apply", mani("v1", "ConfigMap", "../secrets/x", "web")},
		{"k8s_apply", mani("v1", "Secret/../ConfigMap", "x", "web")},
		{"k8s_apply", mani("v1/../../api/v1/namespaces/kube-system", "ConfigMap", "x", "web")},
		{"k8s_apply", mani("v1", "ConfigMap", "x", "kube-system/secrets")},
	} {
		before := len(sent())
		_, evs := runOne(t, reg, pol, sess, c.tool, c.args)
		refused := false
		for _, ev := range evs {
			refused = refused || (ev.Type == agent.EvActionDenied && strings.Contains(string(ev.Payload), `"step":"args"`))
		}
		if !refused || len(sent()) != before {
			t.Errorf("%s %s: refused at args %v, sent %v", c.tool, c.args, refused, sent()[before:])
		}
	}
	// Defence in depth: the tool refuses the same when called directly.
	before := len(sent())
	if res := (GetTool{M: mgr}).Run(context.Background(), sess, json.RawMessage(`{"cluster":"lab","namespace":"kube-system/secrets?","resource":"pods"}`)); !res.IsError || len(sent()) != before {
		t.Fatalf("a direct call with a path in its namespace ran: %s", res.Content)
	}
	// A selector is query-encoded, and a plain call goes where it names.
	_, _ = runOne(t, reg, pol, sess, "k8s_get", `{"cluster":"lab","namespace":"web","resource":"pods","selector":"app=x&watch=true/../secrets"}`)
	last := sent()[len(sent())-1]
	if last != "GET /api/v1/namespaces/web/pods?labelSelector=app=x%26watch=true%2F..%2Fsecrets" {
		t.Fatalf("the selector was not sent encoded: %s", last)
	}
}

// An allow on one namespace never covers a cluster-scoped object: deleting a
// namespace or a node, or applying a ClusterRoleBinding, asks under
// allow k8s_apply(lab/dev/*) whatever namespace the call names, while a
// namespaced object in dev is still allowed. A kind whose scope is unknown
// is refused.
func TestNamespaceAllowNeverCoversClusterScopedObjects(t *testing.T) {
	srv, ca, sent := pathLog(t)
	mgr := NewManager(Config{Kubeconfig: t.TempDir() + "/missing",
		Clusters: []LoginCluster{{Name: "lab", Server: srv.URL, CAFile: ca}}})
	reg := tools.NewRegistry(GetTool{M: mgr}, ApplyTool{M: mgr})
	sess := newSession(t)
	login, _ := json.Marshal(map[string]string{"cluster": "lab", "token_secret": "T", "namespace": "dev"})
	if res := (LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}).Run(context.Background(), sess, login); res.IsError {
		t.Fatal(res.Content)
	}
	apply := func(kind, apiVersion, ns string) string {
		meta := map[string]string{"name": "pwn"}
		if ns != "" {
			meta["namespace"] = ns
		}
		m, _ := json.Marshal(map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta})
		a, _ := json.Marshal(map[string]string{"action": "apply", "manifest": string(m)})
		return string(a)
	}
	// step reports how the call was settled: the step of its approval or denial.
	step := func(args string) (string, []string, []agent.Event) {
		t.Helper()
		pol := policy.New(policy.ModeDefault)
		if err := pol.AddAllow("k8s_apply(lab/dev/*)"); err != nil {
			t.Fatal(err)
		}
		ap := &asked{}
		store := agent.NewMemStore()
		l := agent.NewLoop(&oneCall{name: "k8s_apply", args: args}, reg, pol, ap, sess,
			agent.NewRecorder(store, "s", ""), agent.DefaultConfig())
		before := len(sent())
		if _, err := l.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		evs, _ := store.Events("s")
		for _, ev := range evs {
			if ev.Type == agent.EvActionApproved || ev.Type == agent.EvActionDenied {
				var d map[string]string
				_ = json.Unmarshal(ev.Payload, &d)
				return d["step"], sent()[before:], evs
			}
		}
		return "", sent()[before:], evs
	}
	for _, args := range []string{
		`{"action":"delete","resource":"namespaces","name":"kube-system","namespace":"dev"}`,
		`{"action":"delete","resource":"namespaces","name":"kube-system"}`,
		`{"action":"delete","resource":"nodes","name":"n1","namespace":"dev"}`,
		apply("ClusterRoleBinding", "rbac.authorization.k8s.io/v1", ""),
		apply("ClusterRoleBinding", "rbac.authorization.k8s.io/v1", "dev"),
	} {
		got, _, evs := step(args)
		if got == "allow" {
			t.Errorf("%s was approved by the rule on namespace dev", args)
		}
		var req agent.ActionRequested
		for _, ev := range evs {
			if ev.Type == agent.EvActionRequested {
				_ = json.Unmarshal(ev.Payload, &req)
			}
		}
		if strings.Contains(string(req.Args), `"namespace"`) {
			t.Errorf("%s: a cluster-scoped call kept a namespace: %s", args, req.Args)
		}
	}
	if got, reqs, _ := step(apply("Widget", "example.com/v1", "dev")); got != "args" || len(reqs) != 0 {
		t.Errorf("an apply of a kind of unknown scope was not refused: %s %v", got, reqs)
	}
	if got, reqs, _ := step(apply("ConfigMap", "v1", "")); got != "allow" || len(reqs) != 1 || !strings.Contains(reqs[0], "/namespaces/dev/configmaps/pwn") {
		t.Errorf("a namespaced apply in dev was not allowed: %s %v", got, reqs)
	}
	if got, _, _ := step(`{"action":"restart","resource":"deployments","name":"web"}`); got != "allow" {
		t.Errorf("a namespaced restart in dev was not allowed: %s", got)
	}
}

// A manifest is decoded once, strictly: a key repeated in any case, at any
// depth, or kind, apiVersion, metadata, name or namespace in another case, is
// refused before any rule reads it and before any request. A struct reader
// and the API server would otherwise see different kinds.
func TestManifestKeysAreReadOneWay(t *testing.T) {
	srv, ca, sent := pathLog(t)
	mgr := NewManager(Config{Kubeconfig: t.TempDir() + "/missing",
		Clusters: []LoginCluster{{Name: "lab", Server: srv.URL, CAFile: ca}}})
	reg := tools.NewRegistry(GetTool{M: mgr}, ApplyTool{M: mgr})
	sess := newSession(t)
	login, _ := json.Marshal(map[string]string{"cluster": "lab", "token_secret": "T", "namespace": "dev"})
	if res := (LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}).Run(context.Background(), sess, login); res.IsError {
		t.Fatal(res.Content)
	}
	for _, m := range []string{
		`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRoleBinding","Kind":"ConfigMap","metadata":{"name":"pwn"}}`,
		`{"apiVersion":"rbac.authorization.k8s.io/v1","Kind":"ConfigMap","kind":"ClusterRoleBinding","metadata":{"name":"pwn"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"pwn","namespace":"dev"},"Metadata":{"name":"pwn","namespace":"kube-system"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"pwn","namespace":"kube-system","Namespace":"dev"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"pwn","Name":"other"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","apiversion":"rbac.authorization.k8s.io/v1","metadata":{"name":"pwn"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","kind":"ConfigMap","metadata":{"name":"pwn"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"pwn","labels":{"a":"1","a":"2"}}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"pwn"},"data":{"k":"1","K":"2"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"pwn"},"x":[{"y":1,"y":2}]}`,
		// A case variant alone is also refused: only the API server's spelling is read.
		`{"apiVersion":"v1","Kind":"ConfigMap","metadata":{"name":"pwn"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","Metadata":{"name":"pwn"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"Name":"pwn"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"pwn","NAMESPACE":"kube-system"}}`,
		"{\"apiVersion\":\"v1\",\"Kind\":\"ClusterRoleBinding\",\"kind\":\"ConfigMap\",\"metadata\":{\"name\":\"pwn\"}}",
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"pwn"}} {}`,
	} {
		args, _ := json.Marshal(map[string]string{"action": "apply", "manifest": m})
		for _, mode := range []policy.Mode{policy.ModeDefault, policy.ModeBypass} {
			pol := policy.New(mode)
			if err := pol.AddAllow("k8s_apply(lab/dev/*)"); err != nil {
				t.Fatal(err)
			}
			ap := &asked{}
			store := agent.NewMemStore()
			l := agent.NewLoop(&oneCall{name: "k8s_apply", args: string(args)}, reg, pol, ap, sess,
				agent.NewRecorder(store, "s", ""), agent.DefaultConfig())
			before := len(sent())
			if _, err := l.Run(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			evs, _ := store.Events("s")
			refused := false
			for _, ev := range evs {
				refused = refused || (ev.Type == agent.EvActionDenied && strings.Contains(string(ev.Payload), `"step":"args"`))
			}
			if !refused || len(ap.args) != 0 || len(sent()) != before {
				t.Errorf("%s: %s: refused at args %v, asked %v, sent %v", mode, m, refused, ap.args, sent()[before:])
			}
		}
		// Defence in depth: the tool refuses the same when called directly.
		direct, _ := json.Marshal(map[string]string{"cluster": "lab", "action": "apply", "manifest": m})
		before := len(sent())
		if res := (ApplyTool{M: mgr}).Run(context.Background(), sess, direct); !res.IsError || len(sent()) != before {
			t.Errorf("direct: %s: applied: %s %v", m, res.Content, sent()[before:])
		}
	}
}

// What policy judged is what is sent: the manifest is put in one encoding
// before the rules read it, and the tool sends exactly those bytes.
func TestAppliedManifestIsTheOneJudged(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv, ca := tlsCluster(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(b))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	mgr := NewManager(Config{Kubeconfig: t.TempDir() + "/missing",
		Clusters: []LoginCluster{{Name: "lab", Server: srv.URL, CAFile: ca}}})
	reg := tools.NewRegistry(GetTool{M: mgr}, ApplyTool{M: mgr})
	sess := newSession(t)
	login, _ := json.Marshal(map[string]string{"cluster": "lab", "token_secret": "T", "namespace": "dev"})
	if res := (LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}).Run(context.Background(), sess, login); res.IsError {
		t.Fatal(res.Content)
	}
	m := `{ "metadata": {"name":"cfg"}, "kind":"ConfigMap", "apiVersion":"v1", "data": {"n": 1.50, "h": "<&>"} }`
	args, _ := json.Marshal(map[string]string{"action": "apply", "manifest": m})
	pol := policy.New(policy.ModeDefault)
	if err := pol.AddAllow("k8s_apply(lab/dev/*)"); err != nil {
		t.Fatal(err)
	}
	store := agent.NewMemStore()
	l := agent.NewLoop(&oneCall{name: "k8s_apply", args: string(args)}, reg, pol, &asked{}, sess,
		agent.NewRecorder(store, "s", ""), agent.DefaultConfig())
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("s")
	var req agent.ActionRequested
	for _, ev := range evs {
		if ev.Type == agent.EvActionRequested {
			_ = json.Unmarshal(ev.Payload, &req)
		}
	}
	var judged struct {
		Manifest string `json:"manifest"`
	}
	_ = json.Unmarshal(req.Args, &judged)
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 || bodies[0] != judged.Manifest {
		t.Fatalf("sent %q, judged %q", bodies, judged.Manifest)
	}
	if want := `{"apiVersion":"v1","data":{"h":"<&>","n":1.50},"kind":"ConfigMap","metadata":{"name":"cfg"}}`; judged.Manifest != want {
		t.Fatalf("judged %q, want %q", judged.Manifest, want)
	}
}

// A name is sent as one escaped path segment.
func TestApplyEscapesTheName(t *testing.T) {
	srv, ca, sent := pathLog(t)
	mgr := NewManager(Config{Kubeconfig: t.TempDir() + "/missing",
		Clusters: []LoginCluster{{Name: "lab", Server: srv.URL, CAFile: ca}}})
	sess := newSession(t)
	login, _ := json.Marshal(map[string]string{"cluster": "lab", "token_secret": "T", "namespace": "dev"})
	if res := (LoginTool{M: mgr, Secret: stored(map[string]string{"T": "tok"})}).Run(context.Background(), sess, login); res.IsError {
		t.Fatal(res.Content)
	}
	args, _ := json.Marshal(map[string]string{"cluster": "lab", "namespace": "dev", "action": "apply",
		"manifest": `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x;y"}}`})
	if res := (ApplyTool{M: mgr}).Run(context.Background(), sess, args); res.IsError {
		t.Fatal(res.Content)
	}
	got := sent()
	if got[len(got)-1] != "PATCH /api/v1/namespaces/dev/configmaps/x%3By?fieldManager=abhed&force=true" {
		t.Fatalf("sent %v", got)
	}
}
