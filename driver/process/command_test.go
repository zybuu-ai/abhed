//go:build !windows

package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

func TestCommandRunsInTheInstance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, _, _, ws := fake(t, sandbox.TierProcess)
	if _, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	if err := driver.Check(d); err != nil || !d.Info().Has(driver.CapCommand) {
		t.Fatalf("command not declared: %v", err)
	}
	cmd, err := driver.Command(ctx, d, "a", driver.CommandRequest{Command: "pwd; echo $ABHED_SANDBOX"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil || string(out) != ws+"\nfake\n" {
		t.Fatalf("output %q %v", out, err)
	}
	if _, err := driver.Command(ctx, d, "a", driver.CommandRequest{Command: "true", Cwd: filepath.Dir(ws)}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("a cwd outside the workspace: %v", err)
	}
	if _, err := driver.Command(ctx, d, "a", driver.CommandRequest{}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("neither a command nor a shell: %v", err)
	}
	if _, err := driver.Command(ctx, d, "nope", driver.CommandRequest{Command: "true"}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("no instance: %v", err)
	}
}

// A command handed out counts as running, and the instance stopping ends it.
func TestStopEndsAHandedOutCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, _, _, ws := fake(t, sandbox.TierProcess)
	if _, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	cmd, err := driver.Command(ctx, d, "a", driver.CommandRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	start := time.Now()
	if _, err := d.Stop(ctx, "a", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("the command ended cleanly; the stop did not end it")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command outlived the instance")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("stop waited out the command")
	}
	if _, err := driver.Command(ctx, d, "a", driver.CommandRequest{Command: "true"}); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("a stopped instance handed out a command: %v", err)
	}
}

func TestCommandShell(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, _, _, ws := fake(t, sandbox.TierProcess)
	if _, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	cmd, err := driver.Command(ctx, d, "a", driver.CommandRequest{Shell: true})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin = strings.NewReader("echo shell-$((1+1))\nexit 0\n")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "shell-2") {
		t.Fatalf("shell %q %v", out, err)
	}
}

// Two instances under one root: each one's commands neither read nor write
// the other's folder, on the host's own sandbox.
func TestHiddenSiblingsOnTheRealSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the host's sandbox")
	}
	ctx := context.Background()
	root := localDir(t)
	a, b := filepath.Join(root, "sbx_a"), filepath.Join(root, "sbx_b")
	for _, dir := range []string{a, b} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(b, "secret.txt"), []byte("b's"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := NewWithOptions(config.Default(), Options{Hide: []string{root}})
	_, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: a, MinTier: "process"})
	if errors.Is(err, driver.ErrUnavailable) {
		if _, statErr := os.Stat("/usr/bin/sandbox-exec"); statErr == nil {
			t.Fatalf("Seatbelt is present but the driver refused: %v", err)
		}
		t.Skipf("no process tier on this host: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Delete(ctx, "a") }()
	run := func(line string) (int, string) {
		t.Helper()
		var out syncBuffer
		res, err := d.Exec(ctx, "a", driver.ExecRequest{Command: line, Stdout: &out, Stderr: &out})
		if err != nil {
			t.Fatal(err)
		}
		return res.ExitCode, out.String()
	}
	if code, out := run("echo mine > own.txt && cat own.txt"); code != 0 || !strings.Contains(out, "mine") {
		t.Fatalf("its own folder: %d %q", code, out)
	}
	if code, out := run("cat " + filepath.Join(b, "secret.txt")); code == 0 || strings.Contains(out, "b's") {
		t.Fatalf("read the other sandbox's file: %d %q", code, out)
	}
	if code, out := run("ls " + b); code == 0 || strings.Contains(out, "secret") {
		t.Fatalf("listed the other sandbox's folder: %d %q", code, out)
	}
	if code, _ := run("echo x > " + filepath.Join(b, "planted.txt")); code == 0 {
		t.Fatal("wrote into the other sandbox's folder")
	}
	if _, err := os.Stat(filepath.Join(b, "planted.txt")); err == nil {
		t.Fatal("a file was planted in the other sandbox's folder")
	}
	// The workspace's own .abhed stays denied after its read is allowed.
	if err := os.Mkdir(filepath.Join(a, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, ".abhed", "state"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := run("cat .abhed/state"); code == 0 || strings.Contains(out, "kept") {
		t.Fatalf("read the workspace's .abhed: %d %q", code, out)
	}
}

// A hidden folder that read-only paths would show is refused, not built.
func TestHiddenUnderAReadOnlyPathIsRefused(t *testing.T) {
	root := localDir(t)
	ws := filepath.Join(root, "sbx_a")
	if err := os.Mkdir(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Sandbox.ReadOnlyPaths = []string{filepath.Dir(root)}
	d := NewWithOptions(cfg, Options{Hide: []string{root}})
	if _, err := d.Create(context.Background(), driver.Spec{ID: "a", Workspace: ws}); !errors.Is(err, driver.ErrUnavailable) ||
		!strings.Contains(err.Error(), "kept apart") {
		t.Fatalf("created under a read-only path: %v", err)
	}
}
