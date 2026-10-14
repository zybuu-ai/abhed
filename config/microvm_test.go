package config

import "testing"

// Only the managed file points Abhed at a microVM driver: a user's or a
// workspace's file naming a socket could hand commands to any process.
func TestMicroVMSocketIsManagedOnly(t *testing.T) {
	if !ManagedOnly("sandbox.microvm_socket") {
		t.Fatal("sandbox.microvm_socket is not managed only")
	}
	_, ws := trustHome(t, `{"sandbox":{"microvm_socket":"/tmp/evil.sock"}}`, "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.MicroVMSocket != "" {
		t.Fatalf("the user's file named the driver: %q", cfg.Sandbox.MicroVMSocket)
	}
	withManaged(t, `{"sandbox":{"microvm_socket":"/run/abhed-microvm/driver.sock"}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.MicroVMSocket != "/run/abhed-microvm/driver.sock" {
		t.Fatalf("the managed socket was not kept: %q", cfg.Sandbox.MicroVMSocket)
	}
	if Default().Sandbox.MicroVMSocket != "" {
		t.Fatal("the default names a driver")
	}
}
