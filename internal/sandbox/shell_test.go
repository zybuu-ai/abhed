package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// typeInto runs an interactive shell on a terminal, types each line into it a
// moment apart, and returns everything the terminal showed.
func typeInto(t *testing.T, s Interactive, cwd string, lines ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := s.Shell(ctx, cwd)
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("start the shell: %v", err)
	}
	defer func() { _ = tty.Close() }()
	var mu sync.Mutex
	var out bytes.Buffer
	read := make(chan struct{})
	go func() {
		defer close(read)
		buf := make([]byte, 4096)
		for {
			n, err := tty.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	for _, l := range lines {
		time.Sleep(300 * time.Millisecond)
		_, _ = tty.Write([]byte(l + "\r"))
	}
	_ = cmd.Wait()
	select {
	case <-read:
	case <-time.After(time.Second):
	}
	mu.Lock()
	defer mu.Unlock()
	return out.String()
}

// The workbench's shell keeps its state from line to line, starts where it is
// told, and is held by the same boundary as a single command: nothing written
// outside the workspace, and the harness's own state out of sight.
func TestProcessSandboxShellIsInteractiveAndConfined(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	s := processSandbox(t, ws, false)
	if err := os.MkdirAll(filepath.Join(ws, stateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, stateDir, "users.json"), []byte("state-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := "/usr/local/abhed-shell-probe.txt"
	out := typeInto(t, s.(Interactive), ws,
		`KEPT=yes`,
		`echo "cwd=$PWD kept=$KEPT"`,
		`echo inside > in.txt`,
		`echo x > `+target+` 2>/dev/null`,
		`cat .abhed/users.json 2>/dev/null; echo`,
		`exit`)
	if !strings.Contains(out, "cwd="+ws+" kept=yes") {
		t.Fatalf("the shell did not start in the workspace or keep its state:\n%s", out)
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "in.txt")); string(got) != "inside\n" {
		t.Fatalf("a write inside the workspace did not land: %q\n%s", got, out)
	}
	if _, err := os.Stat(target); err == nil {
		_ = os.Remove(target)
		t.Fatalf("ESCAPE: the shell wrote outside the workspace to %s", target)
	}
	if strings.Contains(out, "state-secret") {
		t.Fatalf("ESCAPE: the shell read the harness's own state\n%s", out)
	}
}

// The no-sandbox tier's shell says so in its prompt.
func TestNoneShellNamesItself(t *testing.T) {
	ws := workspace(t)
	out := typeInto(t, NewNone(DefaultPolicy(ws)), ws, `echo "tier=$ABHED_SANDBOX"`, `exit`)
	if !strings.Contains(out, "(no sandbox)") || !strings.Contains(out, "tier=none") {
		t.Fatalf("the host shell did not name itself:\n%s", out)
	}
}

// The container engine's CLI needs the host's environment (PATH, HOME,
// DOCKER_HOST) to find the engine at all. It once ran with only TERM, because
// the terminal appended to an environment the command had left unset.
func TestContainerCommandKeepsTheHostEnvironment(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///tmp/abhed-probe.sock")
	ws := workspace(t)
	c := &Container{policy: DefaultPolicy(ws), runtime: "docker"}
	for name, env := range map[string][]string{
		"command": c.Command(context.Background(), ws, "true").Env,
		"shell":   c.Shell(context.Background(), ws).Env,
	} {
		if !slices.Contains(env, "DOCKER_HOST=unix:///tmp/abhed-probe.sock") || !slices.Contains(env, "PATH="+os.Getenv("PATH")) {
			t.Errorf("%s: the engine's CLI lost the host environment: %d entries", name, len(env))
		}
	}
	args := c.Shell(context.Background(), ws).Args
	if !slices.Contains(args, "-t") || !slices.Contains(args, "--network") || !slices.Contains(args, "--read-only") {
		t.Errorf("the shell's container is missing its terminal or its confinement: %v", args)
	}
}
