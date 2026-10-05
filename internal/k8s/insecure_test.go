package k8s

import (
	"os"
	"path/filepath"
	"testing"
)

// The kubeconfig's insecure-skip-tls-verify is read for the context in use,
// without opening a connection.
func TestInsecureContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	kc := `apiVersion: v1
kind: Config
current-context: lab
contexts:
- name: lab
  context:
    cluster: lab
    user: me
- name: prod
  context:
    cluster: prod
    user: me
clusters:
- name: lab
  cluster:
    server: https://10.0.0.1:6443
    insecure-skip-tls-verify: true
- name: prod
  cluster:
    server: https://prod.example:6443
users:
- name: me
  user:
    token: x
`
	if err := os.WriteFile(path, []byte(kc), 0o600); err != nil {
		t.Fatal(err)
	}
	if name, insecure := InsecureContext(Config{Kubeconfig: path}); name != "lab" || !insecure {
		t.Fatalf("current context: %q %v", name, insecure)
	}
	if _, insecure := InsecureContext(Config{Kubeconfig: path, Context: "prod"}); insecure {
		t.Fatal("prod verifies TLS")
	}
	if _, insecure := InsecureContext(Config{Kubeconfig: filepath.Join(t.TempDir(), "none")}); insecure {
		t.Fatal("a missing kubeconfig was insecure")
	}
}
