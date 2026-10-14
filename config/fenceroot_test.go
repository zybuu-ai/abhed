package config

import "testing"

// Only the managed file lets the fence run as root.
func TestFenceAllowRootIsManagedOnly(t *testing.T) {
	if !ManagedOnly("fence.allow_root") {
		t.Fatal("fence.allow_root is not managed only")
	}
	_, ws := trustHome(t, `{"fence":{"allow_root":true}}`, `{"fence":{"allow_root":true}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fence.AllowRoot {
		t.Fatal("a user's or workspace's file let the fence run as root")
	}
	withManaged(t, `{"fence":{"allow_root":true}}`)
	if cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true}); err != nil || !cfg.Fence.AllowRoot {
		t.Fatalf("the managed setting was not kept: %v %v", cfg.Fence.AllowRoot, err)
	}
}
