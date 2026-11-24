package hostgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cache helper's default socket lies in ~/.cache, a writable area when caches are shared.
func TestE2CacheDefaultSocket(t *testing.T) {
	r := newRemoteRig(t)
	area := filepath.Join(r.home, ".cache")
	old := writableAreas
	writableAreas = func() []string { return []string{area} }
	t.Cleanup(func() { writableAreas = old })
	t.Setenv("XDG_CACHE_HOME", "")
	run(t, r.home, "config", "--global", "credential.helper", "cache")
	_, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch")
	t.Logf("cache with default socket in %s: err=%v", area, err)
	if err == nil {
		t.Errorf("CACHE HELPER WHOSE DEFAULT SOCKET IS IN A WRITABLE AREA WAS TRUSTED")
	}
}

// The cache and store helpers' default files: the cache's socket in
// ~/.git-credential-cache where it exists (else the XDG cache), the store's
// ~/.git-credentials and XDG file; each judged where no --socket or --file
// names another, which is judged instead.
func TestE2HelperDefaults(t *testing.T) {
	r := newRemoteRig(t)
	cacheArea := filepath.Join(r.home, ".cache")
	old := writableAreas
	writableAreas = func() []string { return []string{cacheArea} }
	t.Cleanup(func() { writableAreas = old })
	t.Setenv("XDG_CACHE_HOME", "")
	ask := func() error {
		_, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch")
		return err
	}
	// ~/.git-credential-cache, the person's own, is where the socket goes.
	if err := os.MkdirAll(filepath.Join(r.home, ".git-credential-cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	run(t, r.home, "config", "--global", "credential.helper", "cache")
	if err := ask(); err != nil {
		t.Fatalf("cache with ~/.git-credential-cache: %v", err)
	}
	// A --socket of the person's own is judged in place of the default.
	_ = os.RemoveAll(filepath.Join(r.home, ".git-credential-cache"))
	run(t, r.home, "config", "--global", "credential.helper", "cache --socket "+filepath.Join(r.home, "sock"))
	if err := ask(); err != nil {
		t.Fatalf("cache with the person's own --socket: %v", err)
	}
	// store's default file, others can write, stops it; with --file of their own it passes.
	creds := filepath.Join(r.home, ".git-credentials")
	if err := os.WriteFile(creds, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(creds, 0o622); err != nil {
		t.Fatal(err)
	}
	run(t, r.home, "config", "--global", "credential.helper", "store")
	if err := ask(); err == nil || !strings.Contains(err.Error(), "keeps credentials in") {
		t.Fatalf("store with its XDG file in the workspace: %v", err)
	}
	run(t, r.home, "config", "--global", "credential.helper", "store --file "+filepath.Join(r.home, "creds"))
	if err := ask(); err != nil {
		t.Fatalf("store with the person's own --file: %v", err)
	}
}
