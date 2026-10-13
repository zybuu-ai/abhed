//go:build !windows

package sandbox

import (
	"context"
	"os"
	"os/exec"
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
	return scriptedEngineWith(t, p, gvisor, "echo runsc\n")
}

// scriptedEngineWith is scriptedEngine whose engine runs script.
func scriptedEngineWith(t *testing.T, p Policy, gvisor bool, script string) *Container {
	t.Helper()
	engine := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
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

// A server whose program the image lacks, or a macOS binary, is refused at
// start, naming the program and both ways out.
func TestContainerServerProgramNotInImage(t *testing.T) {
	t.Setenv("HOME", workspace(t))
	ws := workspace(t)
	macho := filepath.Join(ws, "macbin")
	if err := os.WriteFile(macho, []byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0, 0, 0}, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		program, script, why string
	}{
		{"npx", `case "$*" in *"command -v"*) exit 3;; esac` + "\necho runsc\n", "is not in the sandbox image"},
		{macho, "echo runsc\n", "is a macOS program"},
	} {
		eng := scriptedEngineWith(t, DefaultPolicy(ws), false, c.script)
		_, err := eng.ServerCommand(context.Background(), "s", []string{c.program}, nil, nil)
		if err == nil {
			t.Fatalf("%s was started", c.program)
		}
		for _, want := range []string{c.program, c.why, "ABHED_SANDBOX_IMAGE", `"host_files": true`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: no %q in %v", c.program, want, err)
			}
		}
	}
}

// realEngineForServers is the container tier on this machine's engine, with
// a home and workspace of the test's own, or skips.
func realEngineForServers(t *testing.T) (*Container, string) {
	t.Helper()
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
	c := NewContainer(DefaultPolicy(ws))
	if ok, why := c.Available(); !ok {
		t.Fatalf("no container engine: %s", why)
	}
	return c, ws
}

// serverContainers lists the containers named with the server prefix.
func serverContainers(t *testing.T, c *Container) string {
	t.Helper()
	out, err := exec.Command(c.runtime, "ps", "-a", "--filter", "name=^"+serverPrefix, "--format", "{{.Names}}").Output() // #nosec G204 -- the test's engine
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// startStubbornServer starts a server that ignores its stdin closing and
// the engine client's end, and waits for its container.
func startStubbornServer(t *testing.T, c *Container) *exec.Cmd {
	t.Helper()
	cmd, err := c.ServerCommand(context.Background(), "stubborn", []string{"/bin/sh", "-c", "trap '' TERM HUP INT; while :; do sleep 1; done"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(60 * time.Second); serverContainers(t, c) == ""; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the server's container never appeared")
		}
	}
	return cmd
}

// A server's container is gone once its cleanup returns, before its engine
// client is killed; and once an exit has ended the servers.
func TestContainerServerRemovedOnCloseOnARealEngine(t *testing.T) {
	c, _ := realEngineForServers(t)
	if os.Getenv("ABHED_SERVER_EXIT_HELPER") == "1" {
		startStubbornServer(t, c)
		EndServers()
		os.Exit(0)
	}
	cmd := startStubbornServer(t, c)
	cleanup := ServerCleanup(cmd)
	if cleanup == nil {
		t.Fatal("no cleanup for a server's container")
	}
	cleanup()
	if left := serverContainers(t, c); left != "" {
		t.Fatalf("the container outlived its cleanup: %s", left)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	// A process that exits through EndServers leaves none behind.
	helper := exec.Command(os.Args[0], "-test.run=^TestContainerServerRemovedOnCloseOnARealEngine$")
	helper.Env = append(os.Environ(), "ABHED_SERVER_EXIT_HELPER=1")
	if out, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	if left := serverContainers(t, c); left != "" {
		t.Fatalf("a container outlived the process that started it: %s", left)
	}
}

// A bare name is the image's to find, and a path outside the container's
// mounts is the image's file: neither is judged by what this host has there,
// such as a macOS python3 on PATH.
func TestContainerServerLeavesBareNamesToTheImage(t *testing.T) {
	t.Setenv("HOME", workspace(t))
	ws := workspace(t)
	bin := t.TempDir()
	macho := []byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0, 0, 0}
	for _, n := range []string{"python3", "node"} {
		if err := os.WriteFile(filepath.Join(bin, n), macho, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	eng := scriptedEngineWith(t, DefaultPolicy(ws), false, "exit 0\n")
	for _, program := range []string{"python3", "node", filepath.Join(bin, "python3")} {
		if _, err := eng.ServerCommand(context.Background(), "s", []string{program}, nil, nil); err != nil {
			t.Errorf("%s was refused by what this host has: %v", program, err)
		}
	}
}

// A server on a real engine reads none of the home or Abhed's state, uses
// its own home and the workspace, and plants nothing.
func TestContainerServerFilesAreConfinedOnARealEngine(t *testing.T) {
	c, ws := realEngineForServers(t)
	probe := newServerProbe(t, ws, true)
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
	if _, err := c.ServerCommand(ctx, "npx", []string{"npx", "-y", "some-server"}, os.Environ(), nil); err == nil || !strings.Contains(err.Error(), "is not in the sandbox image") {
		t.Fatalf("a program the image lacks was not refused at start: %v", err)
	}
}
