//go:build !windows

package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
	"github.com/zybuu-ai/abhed/driver/plugin"
)

func TestDoctorDriversLine(t *testing.T) {
	var out bytes.Buffer
	cfg := config.Default()
	if printDrivers(&out, cfg) || out.Len() != 0 {
		t.Fatalf("an unconfigured driver was reported: %q", out.String())
	}

	cfg.Sandbox.MicroVMSocket = "/nonexistent/abhed-microvm.sock"
	if !printDrivers(&out, cfg) || !strings.Contains(out.String(), "microvm     UNAVAILABLE") {
		t.Fatalf("an unreachable driver: %q", out.String())
	}
	var f configCheck
	f.driverDown = true
	out.Reset()
	if doctorVerdict(&out, f) != 1 || !strings.Contains(out.String(), "sandbox driver cannot be reached") {
		t.Fatalf("verdict %q", out.String())
	}

	dir, err := os.MkdirTemp("/tmp", "mvmd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := plugin.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- plugin.Serve(ctx, l, drivertest.New("microvm", driver.CapAttach)) }()
	defer func() { cancel(); <-served }()
	cfg.Sandbox.MicroVMSocket = sock
	out.Reset()
	if printDrivers(&out, cfg) || !strings.Contains(out.String(), "microvm     ready") || !strings.Contains(out.String(), "default stays process") {
		t.Fatalf("a reachable driver: %q", out.String())
	}
}
