package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/secrets"
)

// A cluster the operator marks insecure is named on stderr at start, as an SSH
// host that skips its host key check is; a verified one is not.
func TestInsecureLoginClusterWarns(t *testing.T) {
	cfg := config.Default()
	cfg.K8s.Enabled = true
	cfg.K8s.Clusters = []config.K8sClusterConfig{
		{Name: "lab", Server: "https://lab.example:6443", InsecureSkipTLSVerify: true},
		{Name: "prod", Server: "https://api.prod.example:6443"},
	}
	var warn bytes.Buffer
	ts := buildInfra(cfg, secrets.Open(filepath.Join(t.TempDir(), "s.json")), &warn)
	if !strings.Contains(warn.String(), `"lab" skips TLS verification`) {
		t.Fatalf("no warning for an insecure cluster: %q", warn.String())
	}
	if strings.Contains(warn.String(), "prod") {
		t.Fatalf("a verified cluster was warned about: %q", warn.String())
	}
	for _, tool := range ts {
		if tool.Name() == "k8s_login" && !strings.Contains(tool.Description(), "lab, prod") {
			t.Fatalf("k8s_login does not list the declared clusters: %s", tool.Description())
		}
	}
}

// The doctor names a kubeconfig cluster that skips TLS verification, and only
// that one.
func TestDoctorFlagsAnInsecureKubeconfigCluster(t *testing.T) {
	srv := doctorEndpoint(t)
	t.Setenv("HOME", t.TempDir())
	doctor := func(insecure bool) string {
		ws := t.TempDir()
		if r, err := filepath.EvalSymlinks(ws); err == nil {
			ws = r
		}
		kube := filepath.Join(t.TempDir(), "config")
		body := "apiVersion: v1\nclusters:\n- cluster:\n    server: https://kube.example:6443\n"
		if insecure {
			body += "    insecure-skip-tls-verify: true\n"
		}
		body += "  name: c\ncontexts:\n- context:\n    cluster: c\n    user: u\n  name: ctx\n" +
			"current-context: ctx\nusers:\n- name: u\n  user:\n    token: t\n"
		if err := os.WriteFile(kube, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := `{"k8s":{"enabled":true,"kubeconfig":"` + kube + `"},"model":{"default":"stub","providers":{"stub":` +
			`{"type":"openai-compatible","base_url":"` + srv.URL + `","model":"m","context_window":8192}}}}`
		_ = os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755)
		if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv(config.TrustEnv, "1") // the test wrote this configuration
		out, _ := stdoutOf(t, func() int { return newApp().doctor(ws) })
		return out
	}
	const line = "the kubeconfig skips TLS verification"
	if out := doctor(true); !strings.Contains(out, line) {
		t.Fatalf("the doctor does not flag an insecure kubeconfig cluster:\n%s", out)
	}
	if out := doctor(false); strings.Contains(out, line) {
		t.Fatalf("the doctor flags a verified kubeconfig cluster:\n%s", out)
	}
}
