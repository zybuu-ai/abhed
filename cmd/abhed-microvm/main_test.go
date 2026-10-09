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
