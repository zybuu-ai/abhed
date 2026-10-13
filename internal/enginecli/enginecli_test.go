//go:build unix

package enginecli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakePodman writes a podman that answers the two local queries with conns as
// its connection list and rootful and sock as the machine's.
func fakePodman(t *testing.T, conns, rootful, sock string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "conns.json"), []byte(conns), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"'system connection') cat '" + filepath.Join(dir, "conns.json") + "' ;;\n" +
		"'machine inspect') echo '" + rootful + " " + sock + "' ;;\n" +
		"*) exit 1 ;;\n" +
		"esac\n"
	path := filepath.Join(dir, "podman")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { // #nosec G306 -- a stub engine that must run
		t.Fatal(err)
	}
	return path
}

// listenSocket is a Unix socket that exists while the test runs.
func listenSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "eng")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "api.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return sock
}

const rootDefault = `[
 {"Name":"m","URI":"ssh://core@127.0.0.1:1/run/user/501/podman/podman.sock","IsMachine":true,"Default":false},
 {"Name":"m-root","URI":"ssh://root@127.0.0.1:1/run/podman/podman.sock","IsMachine":true,"Default":true}
]`

const rootlessDefault = `[
 {"Name":"m","URI":"ssh://core@127.0.0.1:1/run/user/501/podman/podman.sock","IsMachine":true,"Default":true},
 {"Name":"m-root","URI":"ssh://root@127.0.0.1:1/run/podman/podman.sock","IsMachine":true,"Default":false}
]`

func hostOf(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "CONTAINER_HOST="); ok {
			return v
		}
	}
	return ""
}

func clean(t *testing.T) {
	t.Helper()
	for _, k := range []string{"CONTAINER_HOST", "CONTAINER_CONNECTION", "DOCKER_HOST"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	old := goos
	goos = "darwin"
	t.Cleanup(func() { goos = old; Reset() })
	Reset()
}

// On a Mac a podman machine's own API socket is used in place of the ssh
// connection, and nothing changes where that could change which engine or
// which user is reached.
func TestEngineEnvPrefersMachineSocket(t *testing.T) {
	clean(t)
	sock := listenSocket(t)
	podman := fakePodman(t, rootDefault, "true", sock)
	if got := hostOf(Env(podman)); got != "unix://"+sock {
		t.Fatalf("CONTAINER_HOST = %q, want the machine's socket %s", got, sock)
	}
	cmd := Command(context.Background(), podman, "ps")
	if hostOf(cmd.Env) != "unix://"+sock {
		t.Fatalf("Command's environment lacks the socket: %v", cmd.Env)
	}

	t.Run("DOCKER_HOST, which podman does not read", func(t *testing.T) {
		clean(t)
		t.Setenv("DOCKER_HOST", "unix:///run/podman/podman.sock")
		if got := hostOf(Env(podman)); got != "unix://"+sock {
			t.Errorf("with DOCKER_HOST set, CONTAINER_HOST = %q, want the machine's socket", got)
		}
	})
	t.Run("a host or connection already set", func(t *testing.T) {
		for _, k := range []string{"CONTAINER_HOST", "CONTAINER_CONNECTION"} {
			clean(t)
			t.Setenv(k, "unix:///elsewhere.sock")
			env := Env(podman)
			if got := hostOf(env); k == "CONTAINER_CONNECTION" && got != "" {
				t.Errorf("with %s set, CONTAINER_HOST = %q", k, got)
			}
			if k == "CONTAINER_HOST" && hostOf(env) != "unix:///elsewhere.sock" {
				t.Errorf("CONTAINER_HOST replaced: %q", hostOf(env))
			}
		}
	})
	t.Run("no socket", func(t *testing.T) {
		clean(t)
		gone := filepath.Join(t.TempDir(), "gone.sock")
		if got := hostOf(Env(fakePodman(t, rootDefault, "true", gone))); got != "" {
			t.Errorf("CONTAINER_HOST = %q for a socket that does not exist", got)
		}
	})
	t.Run("rootless machine and default connection", func(t *testing.T) {
		clean(t)
		if got := hostOf(Env(fakePodman(t, rootlessDefault, "false", sock))); got != "unix://"+sock {
			t.Errorf("a rootless machine's own connection: CONTAINER_HOST = %q, want its socket", got)
		}
	})
	t.Run("rootfulness differs", func(t *testing.T) {
		clean(t)
		if got := hostOf(Env(fakePodman(t, rootlessDefault, "true", sock))); got != "" {
			t.Errorf("a rootless default was swapped for the rootful socket: %q", got)
		}
		clean(t)
		if got := hostOf(Env(fakePodman(t, rootDefault, "false", sock))); got != "" {
			t.Errorf("a root connection was given a rootless machine's socket: %q", got)
		}
		clean(t)
		odd := `[{"Name":"m","URI":"ssh://root@127.0.0.1:1/run/podman/podman.sock","IsMachine":true,"Default":true}]`
		if got := hostOf(Env(fakePodman(t, odd, "false", sock))); got != "" {
			t.Errorf("a root URI on the rootless connection's name was given a socket: %q", got)
		}
	})
	t.Run("not a Mac or Windows", func(t *testing.T) {
		clean(t)
		goos = "linux"
		if got := hostOf(Env(podman)); got != "" {
			t.Errorf("on Linux CONTAINER_HOST = %q", got)
		}
	})
	t.Run("not podman", func(t *testing.T) {
		clean(t)
		docker := filepath.Join(filepath.Dir(podman), "docker")
		if err := os.Link(podman, docker); err != nil {
			t.Fatal(err)
		}
		if got := hostOf(Env(docker)); got != "" {
			t.Errorf("docker given CONTAINER_HOST = %q", got)
		}
	})
}

// The machine is asked once per process, not at each command.
func TestEngineEnvAsksOnce(t *testing.T) {
	clean(t)
	sock := listenSocket(t)
	podman := fakePodman(t, rootDefault, "true", sock)
	log := filepath.Join(t.TempDir(), "calls")
	raw, _ := os.ReadFile(podman)
	wrapped := strings.Replace(string(raw), "#!/bin/sh\n", "#!/bin/sh\necho \"$*\" >> '"+log+"'\n", 1)
	if err := os.WriteFile(podman, []byte(wrapped), 0o755); err != nil { // #nosec G306 -- a stub engine that must run
		t.Fatal(err)
	}
	for range 5 {
		Env(podman)
	}
	b, _ := os.ReadFile(log)
	if n := len(strings.Split(strings.TrimSpace(string(b)), "\n")); n != 2 {
		t.Fatalf("%d calls to the engine for five environments:\n%s", n, b)
	}
}

// The guard sees every engine command before it runs and can refuse it.
func TestCommandGuard(t *testing.T) {
	clean(t)
	var seen []string
	Guard = func(path string) error { seen = append(seen, path); return os.ErrPermission }
	t.Cleanup(func() { Guard = nil })
	cmd := Command(context.Background(), "/no/such/podman", "info")
	if cmd.Err == nil || !slices.Contains(seen, "/no/such/podman") {
		t.Fatalf("guard not applied: err %v, seen %v", cmd.Err, seen)
	}
}
