package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOptionsRefuseUnknownKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(`{"kernel":"/k","rootfs":"/r","state_dir":"/s","network":"host"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOptions(p); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("an unknown key was accepted: %v", err)
	}
	if err := os.WriteFile(p, []byte(`{"kernel":"/k","rootfs":"/r","state_dir":"/s","helper":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := loadOptions(p)
	if err != nil || o.Helper == nil || o.Kernel != "/k" {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestRunNeedsConfigAndSocket(t *testing.T) {
	if err := run(nil); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("ran without -config and -socket: %v", err)
	}
}

// An id past uint32 would wrap, 4294967296 to uid 0, and give the image to root.
func TestRunRefusesAnIDThatWouldWrap(t *testing.T) {
	for _, ids := range [][2]string{{"4294967296", "1000"}, {"1000", "4294967296"}, {"4294967295", "1000"}, {"1000", "-1"}, {"0", "1000"}} {
		err := run([]string{"-config", "/nonexistent.json", "-socket", "/nonexistent.sock", "-client-uid", ids[0], "-client-gid", ids[1]})
		if err == nil || !strings.Contains(err.Error(), "each from 1 to 4294967294") {
			t.Errorf("uid %s gid %s: %v", ids[0], ids[1], err)
		}
	}
	// Valid ids reach the settings file.
	err := run([]string{"-config", "/nonexistent.json", "-socket", "/nonexistent.sock", "-client-uid", "1000", "-client-gid", "1000"})
	if err == nil || strings.Contains(err.Error(), "each from") {
		t.Errorf("valid ids: %v", err)
	}
}
