package app

import (
	"bytes"
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
