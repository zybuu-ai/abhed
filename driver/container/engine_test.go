package container

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/egress"
)

// realDriver is the driver named by ABHED_REQUIRE_CONTAINER_DRIVER (docker
// or podman) on this machine's engine. It runs only with that set, as the CI
// job sets it, since it starts containers; there a missing engine fails.
// Containers are named abhedtest-* and removed when the test ends.
func realDriver(t *testing.T, opts Options) *Driver {
	t.Helper()
	name := os.Getenv("ABHED_REQUIRE_CONTAINER_DRIVER")
	if name == "" {
		t.Skip("set ABHED_REQUIRE_CONTAINER_DRIVER=docker or =podman to run the container drivers on a real engine")
	}
	if name != Docker && name != Podman {
		t.Fatalf("ABHED_REQUIRE_CONTAINER_DRIVER=%q: want docker or podman", name)
	}
	if opts.NamePrefix == "" {
		opts.NamePrefix = "abhedtest-"
	}
	if img := os.Getenv("ABHED_SANDBOX_IMAGE"); img == "" && opts.Image == "" {
		opts.Image = "docker.io/library/debian:bookworm-slim"
	}
	cfg := config.Default()
	cfg.Sandbox.MaxMemoryMB, cfg.Sandbox.MaxProcs = 256, 64
	d, err := newFromConfig(name, cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// realWorkspace is a workspace folder the engine can mount: on macOS, the
// engine's VM shares the home folder and the temp folders.
func realWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	return ws
}

func create(t *testing.T, d *Driver, spec driver.Spec) driver.Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	st, err := d.Create(ctx, spec)
	t.Cleanup(func() {
		_ = d.Delete(context.Background(), spec.ID)
		_ = d.remove(d.opts.prefix() + spec.ID)
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return st
}

func run(t *testing.T, d *Driver, id, command string) (string, int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var out bytes.Buffer
	res, err := d.Exec(ctx, id, driver.ExecRequest{Command: command, Stdout: &out, Stderr: &out})
	return out.String(), res.ExitCode, err
}

// One long-lived container holds every boundary its arguments claim, as the
// kernel inside it reports them, and keeps state between commands only in
// the workspace.
func TestContainerDriverOnARealEngine(t *testing.T) {
	d := realDriver(t, Options{})
	ws := realWorkspace(t)
	st := create(t, d, driver.Spec{ID: "sbx_engine01", Generation: 1, Workspace: ws, MinTier: "container"})
	if st.Tier != "container" || st.Phase != driver.PhaseRunning {
		t.Fatalf("status %+v", st)
	}
	t.Logf("backend %s: %s", st.Backend, st.Message)

	if out, code, err := run(t, d, "sbx_engine01", "echo inside > made.txt"); err != nil || code != 0 {
		t.Fatalf("a workspace write: %d %v %s", code, err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "made.txt")); strings.TrimSpace(string(b)) != "inside" {
		t.Fatalf("the workspace write did not reach the host: %q", b)
	}
	if _, code, err := run(t, d, "sbx_engine01", "exit 7"); err != nil || code != 7 {
		t.Fatalf("an exit code: %d %v", code, err)
	}
	if out, code, _ := run(t, d, "sbx_engine01", "echo x > /etc/abhed-test"); code == 0 {
		t.Fatalf("the root filesystem was writable: %s", out)
	}
	if out, code, _ := run(t, d, "sbx_engine01", "printf '#!/bin/sh\\necho ran\\n' > /tmp/x && chmod +x /tmp/x && /tmp/x"); code == 0 || strings.Contains(out, "ran") {
		t.Fatalf("/tmp was executable: %s", out)
	}
	out, _, err := run(t, d, "sbx_engine01", "grep -E '^(Seccomp|NoNewPrivs|CapEff|CapBnd|Uid):' /proc/self/status; cat /proc/net/dev; cat /proc/sys/kernel/hostname; id -u")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Seccomp:\t2", "NoNewPrivs:\t1", "CapEff:\t0000000000000000", "CapBnd:\t0000000000000000", "abhed\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "eth0") {
		t.Errorf("a network device besides lo:\n%s", out)
	}
	if strings.HasSuffix(strings.TrimSpace(out), "\n0") {
		t.Errorf("commands run as root:\n%s", out)
	}
	for file, want := range map[string]string{
		"/sys/fs/cgroup/pids.max":        "64",
		"/sys/fs/cgroup/memory.max":      "268435456",
		"/sys/fs/cgroup/memory.swap.max": "0",
	} {
		out, _, err := run(t, d, "sbx_engine01", "cat "+file)
		if err != nil || strings.TrimSpace(out) != want {
			t.Errorf("%s = %q (%v), want %s", file, strings.TrimSpace(out), err, want)
		}
	}

	// Abhed's state is hidden: unreadable inside, and no write reaches the host.
	state := filepath.Join(ws, ".abhed")
	if err := os.WriteFile(filepath.Join(state, "secret"), []byte("host-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The container was started before the file existed; a fresh one sees the folder as it is now.
	if _, err := d.Stop(context.Background(), "sbx_engine01", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, d, "sbx_engine01", "true"); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("a command on a stopped sandbox: %v", err)
	}
	if st, err := d.Start(context.Background(), "sbx_engine01"); err != nil || st.Phase != driver.PhaseRunning {
		t.Fatalf("start: %+v %v", st, err)
	}
	if out, _, _ := run(t, d, "sbx_engine01", "cat .abhed/secret; echo y > .abhed/made; ls .abhed"); strings.Contains(out, "host-state") {
		t.Fatalf(".abhed was readable inside: %q", out)
	}
	if _, err := os.Stat(filepath.Join(state, "made")); err == nil {
		t.Error("a file made in .abhed inside reached the host")
	}
	// The workspace outlives the container; /tmp does not.
	if out, _, _ := run(t, d, "sbx_engine01", "cat made.txt"); strings.TrimSpace(out) != "inside" {
		t.Errorf("the workspace after a restart: %q", out)
	}

	// A cancelled command is ended inside the container, not only its CLI.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err = d.Exec(ctx, "sbx_engine01", driver.ExecRequest{Command: "sleep 300 & sleep 301; echo never"})
	cancel()
	if err == nil {
		t.Fatal("a cancelled command reported success")
	}
	if out, _, _ := run(t, d, "sbx_engine01", "for p in /proc/[0-9]*; do cat $p/cmdline 2>/dev/null | tr '\\0' ' '; echo; done"); strings.Contains(out, "sleep 30") {
		t.Errorf("the cancelled command's processes still run:\n%s", out)
	}

	if err := d.Delete(context.Background(), "sbx_engine01"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.r.output(context.Background(), []string{"container", "inspect", d.opts.prefix() + "sbx_engine01"}, nil); !notFound(err) {
		t.Fatalf("the container outlived its delete: %v", err)
	}
	if err := d.Delete(context.Background(), "sbx_engine01"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("a second delete: %v", err)
	}
}

// Mounts, the terminal and copying, on a real engine.
func TestContainerDriverMountsAttachCopyOnARealEngine(t *testing.T) {
	extra := realWorkspace(t)
	if err := os.WriteFile(filepath.Join(extra, "ref.txt"), []byte("reference"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := realDriver(t, Options{Mounts: []Mount{{Host: extra, Target: "/ref"}}})
	ws := realWorkspace(t)
	create(t, d, driver.Spec{ID: "sbx_engine02", Generation: 1, Workspace: ws})

	if out, _, _ := run(t, d, "sbx_engine02", "cat /ref/ref.txt"); strings.TrimSpace(out) != "reference" {
		t.Errorf("the extra mount was not readable: %q", out)
	}
	if out, code, _ := run(t, d, "sbx_engine02", "echo x > /ref/new"); code == 0 {
		t.Errorf("the extra mount was writable: %s", out)
	}

	// A terminal: what is typed runs, and the shell's exit code comes back.
	var term bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := d.Attach(ctx, "sbx_engine02", driver.AttachRequest{
		Stdin: strings.NewReader("echo attached-$((40+2))\nexit 3\n"), Stdout: &term,
		Size: driver.WinSize{Rows: 24, Cols: 80},
	})
	if err != nil || res.ExitCode != 3 || !strings.Contains(term.String(), "attached-42") {
		t.Fatalf("attach: %+v %v\n%s", res, err, term.String())
	}

	// Copy in, then out again, through the engine as the sandbox's user.
	var in bytes.Buffer
	tw := tar.NewWriter(&in)
	_ = tw.WriteHeader(&tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "dir/a.txt", Typeflag: tar.TypeReg, Mode: 0o4755, Size: 5})
	_, _ = tw.Write([]byte("hello"))
	_ = tw.Close()
	if err := d.CopyIn(context.Background(), "sbx_engine02", ws, &in); err != nil {
		t.Fatalf("copy in: %v", err)
	}
	info, err := os.Stat(filepath.Join(ws, "dir", "a.txt"))
	if err != nil {
		t.Fatalf("the copied file is not in the workspace: %v", err)
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Error("a setuid bit survived the copy")
	}
	if uid, _ := owner(filepath.Join(ws, "dir", "a.txt")); uid != os.Getuid() {
		t.Errorf("the copied file is owned by %d, not %d", uid, os.Getuid())
	}
	var out bytes.Buffer
	if err := d.CopyOut(context.Background(), "sbx_engine02", filepath.Join(ws, "dir"), &out); err != nil {
		t.Fatalf("copy out: %v", err)
	}
	tr := tar.NewReader(&out)
	found := false
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Name == "dir/a.txt" {
			b, _ := io.ReadAll(tr)
			found = string(b) == "hello"
		}
	}
	if !found {
		t.Error("copy out did not return dir/a.txt")
	}
	// A link out of the workspace is not followed.
	if err := os.Symlink("/etc", filepath.Join(ws, "out")); err != nil {
		t.Fatal(err)
	}
	if err := d.CopyOut(context.Background(), "sbx_engine02", filepath.Join(ws, "out"), io.Discard); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("copy out through a link out of the workspace: %v", err)
	}
}

// On a host whose engine runs in a VM, the allowlist is refused rather
// than the sandbox given the network or none silently.
func TestContainerDriverAllowlistOnARealEngine(t *testing.T) {
	name := os.Getenv("ABHED_REQUIRE_CONTAINER_DRIVER")
	if name == "" {
		t.Skip("set ABHED_REQUIRE_CONTAINER_DRIVER=docker or =podman to run the container drivers on a real engine")
	}
	cfg := config.Default()
	cfg.Sandbox.Network = config.NetworkAllowlist
	cfg.Egress.Rules = []egress.Rule{{Host: "example.com", Decision: "allow"}}
	d, err := newFromConfig(name, cfg, Options{NamePrefix: "abhedtest-", Image: "docker.io/library/debian:bookworm-slim"})
	if err != nil {
		t.Fatal(err)
	}
	ws := realWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_, err = d.Create(ctx, driver.Spec{ID: "sbx_engine03", Generation: 1, Workspace: ws})
	t.Cleanup(func() { _ = d.Delete(context.Background(), "sbx_engine03") })
	if hostOS != "linux" {
		if !errors.Is(err, driver.ErrUnavailable) {
			t.Fatalf("the allowlist on an engine in a VM: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("create under the allowlist: %v", err)
	}
	// Nothing leaves directly; the proxy is reached through the relay, with
	// this command's credential, and refuses a host no rule names.
	out, _, err := run(t, d, "sbx_engine03", `bash -c '
exec 3<>/dev/tcp/1.1.1.1/80 && echo direct-open || echo direct-refused
p=${HTTP_PROXY##*@}; p=${p%/}; cred=${HTTP_PROXY#http://}; cred=${cred%@*}
exec 4<>/dev/tcp/${p%:*}/${p##*:} || { echo relay-closed; exit 0; }
printf "CONNECT denied.example:443 HTTP/1.1\r\nHost: denied.example:443\r\nProxy-Authorization: Basic %s\r\n\r\n" "$(printf %s "$cred" | base64 -w0)" >&4
head -1 <&4' 2>&1`)
	if err != nil || !strings.Contains(out, "direct-refused") || !strings.Contains(out, " 403") {
		t.Errorf("under the allowlist: %v\n%s", err, out)
	}
}
