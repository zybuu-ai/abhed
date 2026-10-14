package config

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func workloadOK() WorkloadAuthConfig {
	return WorkloadAuthConfig{Enabled: true, TrustDomain: "example.org", Audience: "abhed",
		JWTBundleFile: "bundle.json",
		Grants:        []WorkloadGrant{{SPIFFEID: "spiffe://example.org/ci/*", Scopes: []string{"sandboxes:read"}}}}
}

func TestWorkloadGrantReadsReusable(t *testing.T) {
	var g WorkloadGrant
	if err := json.Unmarshal([]byte(`{"spiffe_id":"spiffe://example.org/x","scopes":["s"],"allow_reusable_svid":true}`), &g); err != nil || !g.AllowReusableSVID {
		t.Fatalf("allow_reusable_svid not read: %+v %v", g, err)
	}
}

func TestWorkloadValidation(t *testing.T) {
	if err := workloadOK().validate(); err != nil {
		t.Fatalf("a full config: %v", err)
	}
	if err := (WorkloadAuthConfig{}).validate(); err != nil {
		t.Fatalf("unset: %v", err)
	}
	cases := map[string]func(*WorkloadAuthConfig){
		"trust_domain":   func(w *WorkloadAuthConfig) { w.TrustDomain = "Example.org" },
		"audience":       func(w *WorkloadAuthConfig) { w.Audience = " " },
		"bundle":         func(w *WorkloadAuthConfig) { w.JWTBundleFile = "" },
		"grants is":      func(w *WorkloadAuthConfig) { w.Grants = nil },
		"not a workload": func(w *WorkloadAuthConfig) { w.Grants[0].SPIFFEID = "spiffe://other.org/ci" },
		"bare domain":    func(w *WorkloadAuthConfig) { w.Grants[0].SPIFFEID = "spiffe://example.org/" },
		"wildcard":       func(w *WorkloadAuthConfig) { w.Grants[0].SPIFFEID = "spiffe://example.org/*/x" },
		"no scopes":      func(w *WorkloadAuthConfig) { w.Grants[0].Scopes = nil },
		"token_minutes":  func(w *WorkloadAuthConfig) { w.TokenMinutes = 61 },
		"reload":         func(w *WorkloadAuthConfig) { w.ReloadSeconds = -1 },
	}
	for name, mutate := range cases {
		w := workloadOK()
		w.Grants = slices.Clone(w.Grants)
		mutate(&w)
		if err := w.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := Default()
	c.Auth.Workload = workloadOK()
	c.Auth.Workload.TrustDomain = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "trust_domain") {
		t.Errorf("Validate does not check auth.workload: %v", err)
	}
}

// The agent must not be able to read the token key, or replace a bundle and
// so mint workload identities.
func TestWorkloadFilesAreState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	c := Default()
	c.Auth.Workload = workloadOK()
	c.Auth.Workload.X509BundleFile = "/etc/abhed/roots.pem"
	got := c.ServerSecretFiles(ws)
	key, _ := c.WorkloadKeyFile(ws)
	for _, want := range []string{key, key + CLIEditionStateSuffix, filepath.Join(ws, "bundle.json"), "/etc/abhed/roots.pem"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s missing from %v", want, got)
		}
	}
	c.Auth.Workload.Enabled = false
	if got := c.ServerSecretFiles(ws); len(got) != 0 {
		t.Errorf("disabled workload identity still lists %v", got)
	}
}
