//go:build !windows

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// scriptedEngine is a container tier whose engine is a script that answers its
// probes, so the arguments a server gets can be checked without one.
func scriptedEngine(t *testing.T, p Policy, gvisor bool) *Container {
	t.Helper()
	engine := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\necho runsc\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewContainer(p)
	if gvisor {
		c = NewGVisor(p)
	}
	c.once.Do(func() {})
	c.runtime, c.available = engine, true
	return c
}

// On the container and VM tiers a server runs in a container of its own: its
// own home mounted as HOME, its install folder read-only, nothing else of the
// home (a read_only_path in it left out), the workspace's .abhed covered, its
// network open outside the allowlist, its environment in a file.
func TestContainerServerArgs(t *testing.T) {
	for _, gvisor := range []bool{false, true} {
		home := workspace(t)
		t.Setenv("HOME", home)
		ws := workspace(t)
		p := DefaultPolicy(ws)
		p.ReadOnlyPaths = []string{filepath.Join(home, "notes"), "/opt/shared"}
		c := scriptedEngine(t, p, gvisor)
		tool := filepath.Join(home, "tools", "srv", "bin", "srv")
		_ = os.MkdirAll(filepath.Dir(tool), 0o755)
		_ = os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd, err := c.ServerCommand(ctx, "srv", []string{tool, "a"}, []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TOKEN=t0k3n-value"}, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		args := strings.Join(cmd.Args, " ")
		own := filepath.Join(home, ".abhed", "mcp", "srv", "home")
		for _, want := range []string{"-v " + own + ":" + own, "-v " + filepath.Join(home, "tools", "srv") + ":" + filepath.Join(home, "tools", "srv") + ":ro",
			"-v /opt/shared:/opt/shared:ro", "-v " + ws + ":" + ws, "--tmpfs " + filepath.Join(ws, ".abhed"), "--cap-drop ALL", "--read-only",
			"--name " + serverPrefix, Image + " " + tool + " a"} {
			if !strings.Contains(args, want) {
				t.Errorf("gvisor %v: no %q in %s", gvisor, want, args)
			}
		}
		if strings.Contains(args, filepath.Join(home, "notes")) || strings.Contains(args, "--network none") || strings.Contains(args, "t0k3n") {
			t.Errorf("gvisor %v: a home path, no network or a value in the arguments: %s", gvisor, args)
		}
		if gvisor != strings.Contains(args, "--runtime runsc") {
			t.Errorf("gvisor %v: runtime in %s", gvisor, args)
		}
		i := slices.Index(cmd.Args, "--env-file")
		if i < 0 {
			t.Fatalf("no env file: %s", args)
		}
		env, _ := os.ReadFile(cmd.Args[i+1])
		if !strings.Contains(string(env), "HOME="+own+"\n") || !strings.Contains(string(env), "TOKEN=t0k3n-value") ||
			strings.Contains(string(env), "HOME="+home+"\n") || strings.Contains(string(env), "PATH=") {
			t.Errorf("gvisor %v: the server's environment:\n%s", gvisor, env)
		}
		cancel()
	}
}

// A server on a real engine reads none of the home or Abhed's state, uses
// its own home and the workspace, and plants nothing.
func TestContainerServerFilesAreConfinedOnARealEngine(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_CONTAINER") == "" {
		t.Skip("set ABHED_REQUIRE_CONTAINER=1 to run a server on a real engine")
	}
	if img := os.Getenv("ABHED_TEST_SERVER_IMAGE"); img != "" {
		old := Image
		Image = img
		t.Cleanup(func() { Image = old })
	}
	if pre := os.Getenv("ABHED_TEST_CONTAINER_PREFIX"); pre != "" {
		old := serverPrefix
		serverPrefix = pre
		t.Cleanup(func() { serverPrefix = old })
	}
	// The engine's CLI finds its connection under the real home.
	if real, err := os.UserHomeDir(); err == nil {
		for k, v := range map[string]string{"XDG_CONFIG_HOME": ".config", "XDG_DATA_HOME": ".local/share"} {
			if os.Getenv(k) == "" {
				t.Setenv(k, filepath.Join(real, v))
			}
		}
	}
	t.Setenv("HOME", workspace(t))
	ws := workspace(t)
	probe := newServerProbe(t, ws, true)
	c := NewContainer(DefaultPolicy(ws))
	if ok, why := c.Available(); !ok {
		t.Fatalf("no container engine: %s", why)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd, err := c.ServerCommand(ctx, "files", probe.argv, os.Environ(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the server did not run: %v\n%s", err, out)
	}
	probe.check(t, "files", string(out))
}
