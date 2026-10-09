//go:build !windows

package process

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// fakeSandbox runs commands on the host with a marker in their environment.
type fakeSandbox struct {
	tier   sandbox.Tier
	closed *atomic.Int32
	refuse error
	caveat string
}

func (f fakeSandbox) Caveat() string { return f.caveat }

func (f fakeSandbox) Tier() sandbox.Tier        { return f.tier }
func (f fakeSandbox) Available() (bool, string) { return true, "" }
func (f fakeSandbox) Describe() string          { return "fake " + string(f.tier) }
func (f fakeSandbox) Backend() string           { return "fake-backend" }
func (f fakeSandbox) Close() error              { f.closed.Add(1); return nil }
func (f fakeSandbox) Command(ctx context.Context, cwd, command string) *exec.Cmd {
	if f.refuse != nil {
		return &exec.Cmd{Err: f.refuse}
	}
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", command)
	cmd.Dir, cmd.Env = cwd, []string{"PATH=/usr/bin:/bin", "ABHED_SANDBOX=fake"}
	return cmd
}
func (f fakeSandbox) Shell(ctx context.Context, cwd string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-i")
	cmd.Dir, cmd.Env = cwd, []string{"PATH=/usr/bin:/bin", "PS1=$ "}
	return cmd
}

func fake(t *testing.T, tier sandbox.Tier) (*Driver, *atomic.Int32, *atomic.Int32, string) {
	t.Helper()
	var built, closed atomic.Int32
	d := newDriver(func(driver.Spec) (sandbox.Sandbox, error) {
		built.Add(1)
		return fakeSandbox{tier: tier, closed: &closed}, nil
	})
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d, &built, &closed, ws
}

func TestInfoPassesCheck(t *testing.T) {
	d, _, _, _ := fake(t, sandbox.TierProcess)
	if err := driver.Check(d); err != nil {
		t.Fatal(err)
	}
	if d.Info().Name != "process" {
		t.Fatal(d.Info().Name)
	}
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	d, built, closed, ws := fake(t, sandbox.TierProcess)
	st, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws, MinTier: "process"})
	if err != nil || st.Tier != "process" || st.Backend != "fake-backend" || st.Phase != driver.PhaseRunning {
		t.Fatalf("create %+v %v", st, err)
	}
	if _, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws}); !errors.Is(err, driver.ErrExists) {
		t.Fatalf("second create %v", err)
	}
	var out bytes.Buffer
	res, err := d.Exec(ctx, "a", driver.ExecRequest{Command: "pwd; echo $ABHED_SANDBOX; cat; exit 4",
		Stdin: strings.NewReader("in"), Stdout: &out})
	if err != nil || res.ExitCode != 4 {
		t.Fatalf("exec %v %v", res, err)
	}
	if out.String() != ws+"\nfake\nin" {
		t.Fatalf("output %q", out.String())
	}
	if st, err := d.Stop(ctx, "a", time.Second); err != nil || st.Phase != driver.PhaseStopped {
		t.Fatalf("stop %+v %v", st, err)
	}
	if closed.Load() != 1 {
		t.Fatal("stop did not close the sandbox")
	}
	if _, err := d.Exec(ctx, "a", driver.ExecRequest{Command: "true"}); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("exec while stopped %v", err)
	}
	if _, err := d.Start(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if built.Load() != 2 {
		t.Fatalf("start built %d sandboxes in all, want 2", built.Load())
	}
	if err := d.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if closed.Load() != 2 {
		t.Fatal("delete did not close the sandbox")
	}
	if _, err := d.Status(ctx, "a"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "a"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestArgvIsQuoted(t *testing.T) {
	ctx := context.Background()
	d, _, _, ws := fake(t, sandbox.TierProcess)
	if _, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := d.Exec(ctx, "a", driver.ExecRequest{Argv: []string{"printf", "%s|", "a b", "$(touch x)", "'"}, Stdout: &out}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "a b|$(touch x)|'|" {
		t.Fatalf("argv output %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "x")); err == nil {
		t.Fatal("an argv element was run as shell")
	}
}

// The minimum tier is enforced: never a weaker sandbox in its place.
func TestMinTier(t *testing.T) {
	ctx := context.Background()
	d, _, closed, ws := fake(t, sandbox.TierNone)
	if _, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws, MinTier: "process"}); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("none accepted for min process: %v", err)
	}
	if closed.Load() != 1 {
		t.Fatal("the refused sandbox was not closed")
	}
	if _, err := d.Status(ctx, "a"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatal("a refused create left an instance")
	}
	if _, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws, MinTier: "strong"}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("unknown tier: %v", err)
	}
	if _, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws, MinTier: "microvm"}); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("min microvm: %v", err)
	}
	d2 := newDriver(func(driver.Spec) (sandbox.Sandbox, error) { return nil, errors.New("no backend") })
	if _, err := d2.Create(ctx, driver.Spec{ID: "a", Workspace: ws}); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("build failure: %v", err)
	}
}

func TestExecRefusals(t *testing.T) {
	ctx := context.Background()
	d, _, _, ws := fake(t, sandbox.TierProcess)
	if _, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]driver.ExecRequest{
		"cwd outside":        {Command: "true", Cwd: "/"},
		"cwd escapes":        {Command: "true", Cwd: "../.."},
		"env replaces":       {Command: "true", Env: []string{"ABHED_SANDBOX=none"}},
		"env replaces PATH":  {Command: "true", Env: []string{"PATH=/tmp"}},
		"env without equals": {Command: "true", Env: []string{"X"}},
		"no command":         {},
	}
	for name, req := range cases {
		if _, err := d.Exec(ctx, "a", req); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A bare entry may be a secret's value: the refusal gives its position only.
	if _, err := d.Exec(ctx, "a", driver.ExecRequest{Command: "true", Env: []string{"A=1", "ghp_secretvalue"}}); err == nil ||
		strings.Contains(err.Error(), "ghp_") || !strings.Contains(err.Error(), "entry 2") {
		t.Errorf("a bare entry: %v", err)
	}
	var out bytes.Buffer
	if _, err := d.Exec(ctx, "a", driver.ExecRequest{Command: "echo $EXTRA", Cwd: ".", Env: []string{"EXTRA=1"}, Stdout: &out}); err != nil || out.String() != "1\n" {
		t.Fatalf("added env %q %v", out.String(), err)
	}
	if _, err := d.Create(ctx, driver.Spec{ID: "b", Workspace: "rel"}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("relative workspace: %v", err)
	}
}

// A sandbox that refuses a command, as the egress proxy does when it cannot
// start, is an error, never an exit code.
func TestRefusedCommandIsAnError(t *testing.T) {
	ctx := context.Background()
	var closed atomic.Int32
	d := newDriver(func(driver.Spec) (sandbox.Sandbox, error) {
		return fakeSandbox{tier: sandbox.TierProcess, closed: &closed, refuse: errors.New("proxy down")}, nil
	})
	ws := t.TempDir()
	if _, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	if res, err := d.Exec(ctx, "a", driver.ExecRequest{Command: "true"}); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("refused command returned %v %v", res, err)
	}
}

func TestStopEndsRunningCommands(t *testing.T) {
	ctx := context.Background()
	d, _, _, ws := fake(t, sandbox.TierProcess)
	if _, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		res driver.ExecResult
		err error
	}
	got := make(chan outcome, 1)
	var out syncBuffer
	go func() {
		res, err := d.Exec(ctx, "a", driver.ExecRequest{Command: "echo up; sleep 30", Stdout: &out})
		got <- outcome{res, err}
	}()
	for !strings.Contains(out.String(), "up") {
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	if _, err := d.Stop(ctx, "a", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	o := <-got
	if o.err != nil || o.res.ExitCode == 0 {
		t.Fatalf("stopped command: %+v %v", o.res, o.err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("stop waited for the command")
	}
}

func TestCancelledExecIsAnError(t *testing.T) {
	d, _, _, ws := fake(t, sandbox.TierProcess)
	if _, err := d.Create(context.Background(), driver.Spec{ID: "a", Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := d.Exec(ctx, "a", driver.ExecRequest{Command: "sleep 30"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled exec returned %v", err)
	}
}

func TestAttach(t *testing.T) {
	ctx := context.Background()
	d, _, _, ws := fake(t, sandbox.TierProcess)
	if _, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	resize := make(chan driver.WinSize, 1)
	resize <- driver.WinSize{Rows: 50, Cols: 120}
	res, err := driver.Attach(ctx, d, "a", driver.AttachRequest{
		Stdin:  strings.NewReader("echo attached-$((40+2))\nexit 7\n"),
		Stdout: &out, Resize: resize, Size: driver.WinSize{Rows: 24, Cols: 80},
	})
	if err != nil || res.ExitCode != 7 {
		t.Fatalf("attach %v %v: %q", res, err, out.String())
	}
	if !strings.Contains(out.String(), "attached-42") {
		t.Fatalf("attach output %q", out.String())
	}
}

// The real driver confines a command as a session's sandbox does: it writes
// the workspace and nothing outside it.
func TestRealSandboxConfines(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the host's sandbox")
	}
	ctx := context.Background()
	// Not under a temp area: the process tier lets commands write those.
	ws, outside := localDir(t), unwritableDir(t)
	cfg := config.Default()
	d := New(cfg)
	st, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws, MinTier: "process"})
	if errors.Is(err, driver.ErrUnavailable) {
		// Fail rather than skip where a process tier is expected to exist.
		if _, statErr := os.Stat("/usr/bin/sandbox-exec"); statErr == nil {
			t.Fatalf("Seatbelt is present but the driver refused: %v", err)
		}
		t.Skipf("no process tier on this host: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Delete(ctx, "a") }()
	if st.Tier == "none" || st.Tier == "" {
		t.Fatalf("tier %q", st.Tier)
	}
	res, err := d.Exec(ctx, "a", driver.ExecRequest{Command: "echo in > inside.txt"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("write inside: %v %v", res, err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "inside.txt")); err != nil || string(b) != "in\n" {
		t.Fatalf("inside file %q %v", b, err)
	}
	target := filepath.Join(outside, "outside.txt")
	res, err = d.Exec(ctx, "a", driver.ExecRequest{Argv: []string{"/bin/sh", "-c", "echo out > " + target}})
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(target); statErr == nil || res.ExitCode == 0 {
		t.Fatalf("a command wrote outside the workspace (exit %d)", res.ExitCode)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func localDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp(".", "abhed128-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	abs, err := filepath.Abs(d)
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// unwritableDir is a new folder outside every area the process tier lets
// commands write, beside the checkout or else in the home folder; with
// neither, as for a checkout and home both under a temp folder, the test is
// skipped saying so.
func unwritableDir(t *testing.T) string {
	t.Helper()
	var parents []string
	if wd, err := os.Getwd(); err == nil {
		parents = append(parents, wd)
	}
	if home, err := os.UserHomeDir(); err == nil {
		parents = append(parents, home)
	}
	writable := func(p string) bool {
		for _, a := range sandbox.WritableAreas() {
			for _, form := range sandbox.PathForms(a) {
				if _, ok := sandbox.Within(sandbox.RealPath(p), form); ok {
					return true
				}
			}
		}
		return false
	}
	for _, p := range parents {
		if writable(p) {
			continue
		}
		d, err := os.MkdirTemp(p, ".abhed128-outside-")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		return sandbox.RealPath(d)
	}
	t.Skipf("the checkout and the home folder %v both lie in areas the sandbox lets commands write (%v), so no folder here is out of reach",
		parents, sandbox.WritableAreas())
	return ""
}

// The process driver cannot hold one sandbox to a size, so a Spec that asks
// for one is refused, never run without it.
func TestConfigIsRefused(t *testing.T) {
	ctx := context.Background()
	d, _, _, ws := fake(t, sandbox.TierProcess)
	_, err := d.Create(ctx, driver.Spec{ID: "a", Workspace: ws, Config: map[string]string{"memory_mb": "512", "cpus": "1"}})
	if !errors.Is(err, driver.ErrInvalid) || !strings.Contains(err.Error(), "cpus, memory_mb") {
		t.Fatalf("a sized spec was not refused: %v", err)
	}
	if _, err := d.Status(ctx, "a"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatal("a refused create left an instance")
	}
}

// The driver reports what the sandbox lacks here, such as bubblewrap with no /proc.
func TestStatusCarriesTheSandboxCaveat(t *testing.T) {
	ctx := context.Background()
	var closed atomic.Int32
	for _, caveat := range []string{"no /proc", ""} {
		d := newDriver(func(driver.Spec) (sandbox.Sandbox, error) {
			return fakeSandbox{tier: sandbox.TierProcess, closed: &closed, caveat: caveat}, nil
		})
		ws, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		st, err := d.Create(ctx, driver.Spec{ID: "sbx_caveat", Workspace: ws})
		if err != nil || st.Caveat != caveat {
			t.Fatalf("create: %+v %v, want caveat %q", st, err, caveat)
		}
		if st, err = d.Status(ctx, "sbx_caveat"); err != nil || st.Caveat != caveat {
			t.Fatalf("status: %+v %v, want caveat %q", st, err, caveat)
		}
		_ = d.Delete(ctx, "sbx_caveat")
	}
}
