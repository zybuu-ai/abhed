//go:build unix

package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// covered reports whether a grant in grants is p or holds it.
func covered(grants []string, p string) bool {
	return slices.ContainsFunc(grants, func(g string) bool { return g == p || strings.HasPrefix(p, g+"/") })
}

// Root is granted what any user may read under /etc, less the password
// hashes, host keys and sudo's rules; a clean folder is granted whole.
func TestRootEtcGrantsHideRootOnlyFiles(t *testing.T) {
	etc := t.TempDir()
	files := map[string]os.FileMode{
		"passwd": 0o644, "shadow": 0o644, "gshadow": 0o640, "secret": 0o600, "sudoers": 0o440,
		"ssh/ssh_config": 0o644, "ssh/ssh_host_ed25519_key": 0o644, "ssh/ssh_host_ed25519_key.pub": 0o644,
		"ssl/certs/ca.pem": 0o644, "ssl/private/key.pem": 0o644, "profile.d/a.sh": 0o644, "sudoers.d/x": 0o644,
		"deep/a/b/c/d/e/f/g/h/i/j/k": 0o644,
	}
	for name, mode := range files {
		p := filepath.Join(etc, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(etc, "ssl", "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shadow", filepath.Join(etc, "link")); err != nil {
		t.Fatal(err)
	}
	grants := rootEtcGrants(etc)
	for _, hidden := range []string{"shadow", "gshadow", "secret", "sudoers", "sudoers.d/x", "ssh/ssh_host_ed25519_key",
		"ssl/private/key.pem", "link", "deep/a/b/c/d/e/f/g/h/i/j/k"} {
		if covered(grants, filepath.Join(etc, hidden)) {
			t.Errorf("%s is granted to root: %v", hidden, grants)
		}
	}
	for _, shown := range []string{"passwd", "ssh/ssh_config", "ssh/ssh_host_ed25519_key.pub", "ssl/certs/ca.pem", "profile.d/a.sh"} {
		if !covered(grants, filepath.Join(etc, shown)) {
			t.Errorf("%s is not granted: %v", shown, grants)
		}
	}
	if covered(grants, etc) || !slices.Contains(grants, filepath.Join(etc, "profile.d")) || !slices.Contains(grants, filepath.Join(etc, "ssl", "certs")) {
		t.Errorf("clean folders are not granted whole, or /etc is: %v", grants)
	}
	clean := t.TempDir()
	if err := os.WriteFile(filepath.Join(clean, "hosts"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := rootEtcGrants(clean); !slices.Equal(got, []string{clean}) {
		t.Errorf("a clean /etc: %v", got)
	}
}

// Run as root, the fence's grants leave /etc/shadow out and /etc/passwd in.
func TestFenceSpecAsRootHidesShadow(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := os.Stat("/etc/shadow"); err != nil {
		t.Skip("no /etc/shadow here")
	}
	s := NewFence(fencePolicy(t)).spec()
	all := slices.Concat(s.Exec, s.Read, s.Write, s.Devices)
	if covered(all, "/etc/shadow") || !covered(all, "/etc/passwd") {
		t.Fatalf("root's grants: %v", s.Exec)
	}
}
