package app

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
)

func writeHostKey(t *testing.T, path string, mode os.FileMode) ssh.PublicKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(pub)
	return sp
}

// control_plane.ssh_host_key is one key replicas share: it must load, and
// one that cannot refuses the start rather than leave a server on a key of
// its own.
func TestConfiguredSSHHostKey(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "relay_key")
	pub := writeHostKey(t, good, 0o600)
	cfg := config.Default()
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.SSHHostKey = good
	var warn bytes.Buffer
	key, err := sandboxSSHKey(cfg, &warn)
	if err != nil || key == nil || !bytes.Equal(key.PublicKey().Marshal(), pub.Marshal()) {
		t.Fatalf("the configured key: %v", err)
	}
	label, err := sshKeyLabel(cfg)
	if err != nil || !strings.Contains(label, good) || !strings.Contains(label, ssh.FingerprintSHA256(pub)) {
		t.Fatalf("label %q %v", label, err)
	}

	loose := filepath.Join(dir, "loose")
	writeHostKey(t, loose, 0o640)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"readable by others": loose, "a link": link, "missing": filepath.Join(dir, "none"),
		"relative": "relay_key"} {
		cfg.ControlPlane.SSHHostKey = path
		if key, err := sandboxSSHKey(cfg, &warn); err == nil || key != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// It is a state file: refused inside the workspace, and denied to commands.
	ws := t.TempDir()
	cfg.ControlPlane.SSHHostKey = filepath.Join(ws, "relay_key")
	if err := sandboxconfig.CheckStatePaths(cfg, ws); err == nil {
		t.Fatal("a host key inside the workspace was accepted")
	}
	cfg.ControlPlane.SSHHostKey = good
	found := false
	for _, p := range sandboxconfig.StatePaths(cfg, ws) {
		found = found || p == good
	}
	if !found {
		t.Fatal("the host key is not a protected state path")
	}
}

// serve -check names the relay's key, and a configured one that cannot be
// read fails it; the server's own key is only named, not made.
func TestServeCheckReportsSSHHostKey(t *testing.T) {
	ws := checkEnv(t, `{"control_plane":{"enabled":true}}`, "")
	var out bytes.Buffer
	if code := newApp().serveCheck(&out, ws, "127.0.0.1:0"); code != 0 || !strings.Contains(out.String(), "made when serve first starts") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if _, path, _ := sshHostKey(config.Default(), false); path != "" {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("serve -check made the host key: %v", err)
		}
	}
	bad := filepath.Join(t.TempDir(), "k")
	writeHostKey(t, bad, 0o644)
	ws = checkEnv(t, `{"control_plane":{"enabled":true,"ssh_host_key":"`+bad+`"}}`, "")
	out.Reset()
	if code := newApp().serveCheck(&out, ws, "127.0.0.1:0"); code != 1 || !strings.Contains(out.String(), "FAIL ssh relay") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
}
