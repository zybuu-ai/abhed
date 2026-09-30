package policy

import (
	"encoding/json"
	"testing"
)

func clusterArgs(kv ...string) json.RawMessage {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	return b
}

var allModes = []Mode{ModeDefault, ModeAcceptEdits, ModeAuto, ModeBypass, ModePlan}

// A deny rule naming a cluster stops a login to it, and every get and apply
// on it, in every mode; the same calls to another cluster are not denied.
func TestDenyOnAClusterHoldsInEveryMode(t *testing.T) {
	for _, mode := range allModes {
		e := New(mode)
		if err := e.AddDeny("k8s_login(prod)", "k8s_get(prod/*)", "k8s_apply(prod/*)"); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			tool    string
			mutates bool
			args    json.RawMessage
		}{
			{"k8s_login", true, clusterArgs("cluster", "prod", "token_secret", "T", "namespace", "demo")},
			{"k8s_login", true, clusterArgs("cluster", "prod", "token_secret", "T")},
			{"k8s_get", false, clusterArgs("cluster", "prod", "resource", "pods")},
			{"k8s_get", false, clusterArgs("cluster", "prod", "resource", "pods", "namespace", "kube-system")},
			{"k8s_apply", true, clusterArgs("cluster", "prod", "action", "delete", "resource", "pods", "name", "x")},
		} {
			if d := e.Evaluate(c.tool, c.mutates, c.args); d.Decision != Deny || d.Step != "deny" {
				t.Errorf("%s: %s %s on prod: %s at %s (%s)", mode, c.tool, c.args, d.Decision, d.Step, d.Reason)
			}
			lab := json.RawMessage(string(c.args))
			var m map[string]string
			_ = json.Unmarshal(lab, &m)
			m["cluster"] = "lab"
			lab, _ = json.Marshal(m)
			if d := e.Evaluate(c.tool, c.mutates, lab); d.Step == "deny" {
				t.Errorf("%s: a rule on prod denied %s on lab: %s", mode, c.tool, d.Reason)
			}
		}
	}
}

// An allow rule on one cluster approves nothing on another, and a rule
// written on a namespace, the old subject, approves no login at all.
func TestAllowOnAClusterCoversOnlyThatCluster(t *testing.T) {
	e := New(ModeDefault)
	if err := e.AddAllow("k8s_login(lab)", "k8s_apply(lab/*)", "k8s_login(demo)"); err != nil {
		t.Fatal(err)
	}
	if d := e.Evaluate("k8s_login", true, clusterArgs("cluster", "lab", "token_secret", "T", "namespace", "demo")); d.Decision != Allow {
		t.Fatalf("allow k8s_login(lab) did not approve a login to lab: %s", d.Reason)
	}
	for _, args := range []json.RawMessage{
		clusterArgs("cluster", "prod", "token_secret", "T", "namespace", "demo"),
		clusterArgs("cluster", "prod", "token_secret", "T"),
	} {
		if d := e.Evaluate("k8s_login", true, args); d.Decision != Ask {
			t.Errorf("a rule on lab, or on namespace demo, approved %s: %s (%s)", args, d.Decision, d.Reason)
		}
	}
	if d := e.Evaluate("k8s_apply", true, clusterArgs("cluster", "lab", "action", "scale")); d.Decision != Allow {
		t.Fatalf("allow k8s_apply(lab/*) did not approve a write to lab: %s", d.Reason)
	}
	for _, args := range []json.RawMessage{
		clusterArgs("cluster", "prod", "action", "scale"),
		clusterArgs("context", "lab", "action", "scale"),
		clusterArgs("action", "scale"),
	} {
		if d := e.Evaluate("k8s_apply", true, args); d.Decision != Ask {
			t.Errorf("allow k8s_apply(lab/*) approved %s: %s", args, d.Reason)
		}
	}
}

// The "always allow" offered names the cluster, so a choice remembered for
// one cluster is never the scope another cluster's call asks under.
func TestAlwaysAllowScopeNamesTheCluster(t *testing.T) {
	e := New(ModeDefault)
	for _, c := range []struct {
		tool       string
		args       json.RawMessage
		want       string
		otherwhere json.RawMessage
	}{
		{"k8s_login", clusterArgs("cluster", "lab", "token_secret", "T", "namespace", "demo"), "k8s_login(lab)",
			clusterArgs("cluster", "prod", "token_secret", "T", "namespace", "demo")},
		{"k8s_apply", clusterArgs("cluster", "lab", "namespace", "demo", "action", "delete"), "k8s_apply(lab/demo/delete)",
			clusterArgs("cluster", "prod", "namespace", "demo", "action", "delete")},
		{"k8s_apply", clusterArgs("context", "lab", "action", "delete"), "k8s_apply(context:lab//delete)",
			clusterArgs("cluster", "lab", "action", "delete")},
	} {
		d := e.Evaluate(c.tool, true, c.args)
		if d.Offer() != c.want {
			t.Errorf("%s %s offered %q, want %q", c.tool, c.args, d.Offer(), c.want)
		}
		if other := e.Evaluate(c.tool, true, c.otherwhere).Offer(); other == d.Offer() || other == "" {
			t.Errorf("%s: %s asks under %q, the scope %s was offered", c.tool, c.otherwhere, other, c.args)
		}
	}
}

// Rules written on a resource, a verb or a namespace, the subjects these
// tools had before, still deny.
func TestOlderClusterRulesStillDeny(t *testing.T) {
	e := New(ModeBypass)
	if err := e.AddDeny("k8s_get(secrets*)", "k8s_apply(delete)", "k8s_login(kube-system)"); err != nil {
		t.Fatal(err)
	}
	for tool, args := range map[string]json.RawMessage{
		"k8s_get":   clusterArgs("cluster", "lab", "resource", "secrets"),
		"k8s_apply": clusterArgs("cluster", "lab", "action", "delete"),
		"k8s_login": clusterArgs("cluster", "lab", "token_secret", "T", "namespace", "kube-system"),
	} {
		if d := e.Evaluate(tool, true, args); d.Decision != Deny {
			t.Errorf("an older rule no longer denies %s %s: %s", tool, args, d.Reason)
		}
	}
}

// A cluster-scoped object is judged under a namespace no rule for a real one
// matches, whatever namespace the call names; so is a kind of unknown scope.
func TestClusterScopedObjectsAreNotInANamespace(t *testing.T) {
	e := New(ModeDefault)
	if err := e.AddAllow("k8s_apply(lab/dev/*)", "k8s_get(lab/dev/*)"); err != nil {
		t.Fatal(err)
	}
	manifest := func(kind string) string {
		b, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": kind, "metadata": map[string]string{"name": "x", "namespace": "dev"}})
		return string(b)
	}
	for _, c := range []struct {
		tool string
		args json.RawMessage
	}{
		{"k8s_apply", clusterArgs("cluster", "lab", "namespace", "dev", "action", "delete", "resource", "namespaces", "name", "kube-system")},
		{"k8s_apply", clusterArgs("cluster", "lab", "namespace", "dev", "action", "delete", "resource", "Node", "name", "n1")},
		{"k8s_apply", clusterArgs("cluster", "lab", "namespace", "dev", "action", "apply", "manifest", manifest("ClusterRoleBinding"))},
		{"k8s_apply", clusterArgs("cluster", "lab", "namespace", "dev", "action", "apply", "manifest", manifest("Widget"))},
		{"k8s_get", clusterArgs("cluster", "lab", "namespace", "dev", "resource", "nodes")},
	} {
		if d := e.Evaluate(c.tool, true, c.args); d.Decision == Allow {
			t.Errorf("%s %s was allowed by a rule on namespace dev", c.tool, c.args)
		}
	}
	if d := e.Evaluate("k8s_apply", true, clusterArgs("cluster", "lab", "namespace", "dev", "action", "apply", "manifest", manifest("ConfigMap"))); d.Decision != Allow {
		t.Errorf("a namespaced apply in dev was not allowed: %s", d.Reason)
	}
}

// A manifest that repeats kind in another case is judged cluster-wide, never
// by the kind a case-insensitive reader would take last.
func TestApplySubjectReadsTheManifestStrictly(t *testing.T) {
	for _, m := range []string{
		`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRoleBinding","Kind":"ConfigMap","metadata":{"name":"pwn"}}`,
		`{"apiVersion":"v1","Kind":"ConfigMap","metadata":{"name":"pwn"}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"pwn","labels":{"a":"1","A":"2"}}}`,
	} {
		args := clusterArgs("cluster", "lab", "namespace", "dev", "action", "apply", "manifest", m)
		if got := Subject("k8s_apply", args); got != "lab/-/apply" {
			t.Errorf("%s judged as %q", m, got)
		}
	}
	ok := clusterArgs("cluster", "lab", "namespace", "dev", "action", "apply", "manifest", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x"}}`)
	if got := Subject("k8s_apply", ok); got != "lab/dev/apply" {
		t.Errorf("a plain ConfigMap judged as %q", got)
	}
}
