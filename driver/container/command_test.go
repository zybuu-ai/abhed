package container

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

func lastExec(f *fakeRunner) []string {
	var out []string
	for _, c := range f.find("exec") {
		if !slices.Contains(c, verifyScript) && !slices.Contains(c, killTree) {
			out = c
		}
	}
	return out
}

// A handed-out command is the engine's exec as the sandbox's user, in a
// folder inside the workspace, with the same wrapper Exec uses.
func TestCommandIsTheEnginesExec(t *testing.T) {
	f := &fakeRunner{podman: true}
	d, ws := testDriver(t, f, Options{}, nil)
	if err := driver.Check(d); err != nil || !d.Info().Has(driver.CapCommand) {
		t.Fatalf("command not declared: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := d.Create(ctx, spec(ws)); err != nil {
		t.Fatal(err)
	}
	id := "sbx_0123456789abcdef"
	cmd, err := driver.Command(ctx, d, id, driver.CommandRequest{Command: "make test", Cwd: "sub"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	got := joined(lastExec(f))
	for _, want := range []string{" exec -i --user ", " -w " + filepath.Join(ws, "sub") + " ", " abhed-sbx-" + id + " /bin/sh -c ", " make test "} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, " -t ") {
		t.Error("a command got a terminal")
	}
	if _, err := driver.Command(ctx, d, id, driver.CommandRequest{Shell: true}); err != nil {
		t.Fatal(err)
	}
	shell := joined(lastExec(f))
	for _, want := range []string{" exec -i -t ", " -e TERM=xterm-256color ", " -e ABHED_TERMINAL=1 "} {
		if !strings.Contains(shell, want) {
			t.Errorf("missing %q in the shell's %s", want, shell)
		}
	}
	for _, dir := range []string{"..", "/etc"} {
		if _, err := driver.Command(ctx, d, id, driver.CommandRequest{Command: "true", Cwd: dir}); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("cwd %s: %v", dir, err)
		}
	}
	if _, err := d.Stop(ctx, id, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Command(ctx, d, id, driver.CommandRequest{Command: "true"}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("a stopped instance handed out a command: %v", err)
	}
}

// Variables reach the container by name only, placed before its name.
func TestForwardEnvByName(t *testing.T) {
	d := &Driver{}
	args := []string{"podman", "exec", "-i", "--user", "1:1", "-w", "/w", "abhed-sbx-x", "/bin/sh", "-c", execWrapper, "abhed-exec", "/tmp/p", "env"}
	cmd := &exec.Cmd{Args: slices.Clone(args), Env: []string{"TOKEN=s3cret"}}
	d.ForwardEnv(cmd, []string{"TOKEN"})
	if got := joined(cmd.Args); !strings.Contains(got, " -w /w -e TOKEN abhed-sbx-x /bin/sh -c ") || strings.Contains(got, "s3cret") {
		t.Fatalf("forwarded %s", got)
	}
	other := &exec.Cmd{Args: []string{"/bin/sh", "-c", "env"}}
	d.ForwardEnv(other, []string{"TOKEN"})
	if len(other.Args) != 3 {
		t.Fatalf("a command not built here was changed: %v", other.Args)
	}
}

// Cancelling a handed-out command ends it inside the container too.
func TestCancelledCommandIsKilledInside(t *testing.T) {
	f := &fakeRunner{podman: true, script: "sleep 30"}
	d, ws := testDriver(t, f, Options{}, nil)
	if _, err := d.Create(context.Background(), spec(ws)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := driver.Command(ctx, d, "sbx_0123456789abcdef", driver.CommandRequest{Command: "sleep 300"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	_ = cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range f.find("exec") {
			if slices.Contains(c, killTree) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no kill inside the container")
}
