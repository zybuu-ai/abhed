//go:build !windows

package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
	"github.com/zybuu-ai/abhed/driver/plugin"
)

func TestDefaultsAreUnchanged(t *testing.T) {
	r, entries, err := Registry(context.Background(), config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Names(), []string{"process"}) || len(entries) != 1 || Default != "process" {
		t.Fatalf("names %v, entries %+v", r.Names(), entries)
	}
}

func TestMicroVMIsSelectableByName(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mvmb")
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

	cfg := config.Default()
	cfg.Sandbox.MicroVMSocket = sock
	r, entries, err := Registry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.Get("microvm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(context.Background(), driver.Spec{ID: "sbx_1"}); err != nil {
		t.Fatalf("create through the configured socket: %v", err)
	}
	if len(entries) != 2 || entries[1].Err != nil {
		t.Fatalf("entries %+v", entries)
	}
}

func TestUnreachableMicroVMFailsClosed(t *testing.T) {
	for _, sock := range []string{"/nonexistent/abhed-microvm.sock", "relative.sock"} {
		cfg := config.Default()
		cfg.Sandbox.MicroVMSocket = sock
		r, entries, err := Registry(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		d, err := r.Get("microvm")
		if err != nil {
			t.Fatalf("an unreachable driver is not registered: %v", err)
		}
		if _, err := d.Create(context.Background(), driver.Spec{ID: "sbx_1"}); !errors.Is(err, driver.ErrUnavailable) || !strings.Contains(err.Error(), "microvm") {
			t.Fatalf("create on an unreachable driver: %v", err)
		}
		if _, err := d.Exec(context.Background(), "sbx_1", driver.ExecRequest{Command: "true"}); !errors.Is(err, driver.ErrUnavailable) {
			t.Fatalf("exec: %v", err)
		}
		if entries[1].Err == nil {
			t.Fatal("doctor would not see it is unreachable")
		}
	}
}
