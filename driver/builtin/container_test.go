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
	"github.com/zybuu-ai/abhed/driver/container"
)

// engine writes a stand-in for an engine's CLI.
func engine(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "engine")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

const podmanAnswers = `case "$1" in --version) echo "podman version 5.2.2";; info) echo "5.2.2 true false";; *) exit 1;; esac`

func TestContainerDriversRegisterWhenEnabled(t *testing.T) {
	cfg := config.Default()
	cfg.ControlPlane.Podman = config.ContainerDriverConfig{Enabled: true, Binary: engine(t, podmanAnswers)}
	r, entries, err := Registry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Names(), []string{"podman", "process"}) {
		t.Fatalf("names %v", r.Names())
	}
	d, _ := r.Get("podman")
	if !d.Info().Has(driver.CapCommand) {
		t.Fatalf("podman info %+v", d.Info())
	}
	if len(entries) != 2 || entries[1].Name != "podman" || entries[1].Err != nil {
		t.Fatalf("entries %+v", entries)
	}
	if Default != "process" {
		t.Fatal("the default moved")
	}
}

// An engine that does not answer leaves its driver registered, refusing
// every sandbox, so choosing it never falls through to another.
func TestUnreachableEngineFailsClosed(t *testing.T) {
	for _, bin := range []string{engine(t, "exit 1"), "/nonexistent/docker"} {
		cfg := config.Default()
		cfg.ControlPlane.Docker = config.ContainerDriverConfig{Enabled: true, Binary: bin}
		r, entries, err := Registry(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		d, err := r.Get("docker")
		if err != nil {
			t.Fatalf("an unreachable engine's driver is not registered: %v", err)
		}
		if len(entries) != 2 || entries[1].Err == nil || !errors.Is(entries[1].Err, driver.ErrUnavailable) {
			t.Fatalf("entries %+v", entries)
		}
		if _, err := d.Create(context.Background(), driver.Spec{ID: "sbx_0123456789abcdef", Workspace: t.TempDir()}); !errors.Is(err, driver.ErrUnavailable) ||
			!strings.Contains(err.Error(), "docker") {
			t.Fatalf("create on an unreachable engine: %v", err)
		}
	}
}

func TestContainerOptionsAreChecked(t *testing.T) {
	cfg := config.Default()
	cfg.ControlPlane.Podman = config.ContainerDriverConfig{Enabled: true, Binary: engine(t, podmanAnswers), GPUs: []string{"/dev/nvidia0"}}
	if _, _, err := Registry(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "control_plane.podman") {
		t.Fatalf("a raw device was accepted: %v", err)
	}
	cfg.ControlPlane.Podman = config.ContainerDriverConfig{Enabled: true, Binary: engine(t, podmanAnswers), SeccompProfile: "unconfined"}
	if _, _, err := Registry(context.Background(), cfg); err == nil {
		t.Fatal("an unconfined seccomp profile was accepted")
	}
	for _, p := range []string{"-x", "a b", "x/y", "a;rm", strings.Repeat("a", 70)} {
		cfg.ControlPlane.Podman = config.ContainerDriverConfig{Enabled: true, Binary: engine(t, podmanAnswers), NamePrefix: p}
		if _, _, err := Registry(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "name prefix") {
			t.Errorf("name prefix %q: %v", p, err)
		}
	}
	cfg.ControlPlane.Podman = config.ContainerDriverConfig{Enabled: true, Binary: engine(t, podmanAnswers), NamePrefix: "ci-sbx-"}
	if _, _, err := Registry(context.Background(), cfg); err != nil {
		t.Errorf("a valid name prefix: %v", err)
	}
}

func TestContainerOptionsFromConfig(t *testing.T) {
	sec := config.ContainerDriverConfig{Binary: "/usr/bin/podman", Image: "alpine:3", Images: []string{"python:3.12"},
		Mounts:     []config.ContainerMount{{Host: "/srv/ref", Target: "/ref", Relabel: true}},
		MountRoots: []config.ContainerMountRoot{{Path: "/srv/data", Writable: true}},
		GPUs:       []string{"nvidia.com/gpu=all"}, CPUs: 4, SeccompProfile: "/etc/abhed/seccomp.json", NamePrefix: "ci-sbx-"}
	o := ContainerOptions(sec, container.Options{SnapshotDir: "/snaps"})
	if o.Binary != sec.Binary || o.Image != sec.Image || !slices.Equal(o.Images, sec.Images) || o.CPUs != 4 ||
		o.SeccompProfile != sec.SeccompProfile || !slices.Equal(o.GPUs, sec.GPUs) || o.SnapshotDir != "/snaps" || o.NamePrefix != "ci-sbx-" {
		t.Fatalf("options %+v", o)
	}
	if len(o.Mounts) != 1 || o.Mounts[0] != (container.Mount{Host: "/srv/ref", Target: "/ref", Relabel: true}) ||
		len(o.MountRoots) != 1 || o.MountRoots[0] != (container.MountRoot{Path: "/srv/data", Writable: true}) {
		t.Fatalf("mounts %+v %+v", o.Mounts, o.MountRoots)
	}
}
