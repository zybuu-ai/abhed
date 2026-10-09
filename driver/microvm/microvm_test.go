//go:build !windows

package microvm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/plugin"
)

const id = "sbx_0123456789abcdef"

func newFake(t *testing.T) (*Driver, *fakeBackend) {
	t.Helper()
	be := &fakeBackend{}
	d := newDriver(testOptions(t), be)
	if err := driver.Check(d); err != nil {
		t.Fatal(err)
	}
	return d, be
}

func execIn(t *testing.T, d driver.Driver, req driver.ExecRequest) (driver.ExecResult, string, error) {
	t.Helper()
	var out bytes.Buffer
	req.Stdout = &out
	res, err := d.Exec(context.Background(), id, req)
	return res, out.String(), err
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	d, be := newFake(t)
	st, err := d.Create(ctx, driver.Spec{ID: id, MinTier: "vm", Config: map[string]string{"vcpus": "2", "memory_mib": "256"}})
	if err != nil {
		t.Fatal(err)
	}
	if st.Tier != Tier || st.Phase != driver.PhaseRunning || st.Backend != "fake" {
		t.Fatalf("status %+v", st)
	}
	if b := be.boots[0]; b.vcpus != 2 || b.memMiB != 256 || b.vmID != "sbx-0123456789abcdef" {
		t.Fatalf("boot %+v", b)
	}
	if _, err := d.Create(ctx, driver.Spec{ID: id}); !errors.Is(err, driver.ErrExists) {
		t.Fatalf("second create: %v", err)
	}

	res, out, err := execIn(t, d, driver.ExecRequest{Command: "echo hi; exit 4"})
	if err != nil || res.ExitCode != 4 || out != "hi\n" {
		t.Fatalf("exec: %+v %q %v", res, out, err)
	}

	m := be.last()
	st, err = d.Stop(ctx, id, 5*time.Second)
	if err != nil || st.Phase != driver.PhaseStopped {
		t.Fatalf("stop: %+v %v", st, err)
	}
	if killed, shutdown := m.counts(); shutdown != 1 || killed != 1 {
		t.Fatalf("stop with grace: shutdown %d, killed %d", shutdown, killed)
	}
	if _, _, err := execIn(t, d, driver.ExecRequest{Command: "true"}); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("exec on a stopped instance: %v", err)
	}

	if st, err := d.Start(ctx, id); err != nil || st.Phase != driver.PhaseRunning || len(be.boots) != 2 {
		t.Fatalf("start: %+v %v", st, err)
	}
	if err := d.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Status(ctx, id); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("status after delete: %v", err)
	}
	if err := d.Delete(ctx, id); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.opts.StateDir, id)); !os.IsNotExist(err) {
		t.Fatalf("state left after delete: %v", err)
	}
}

func TestUnavailableBackendFailsClosed(t *testing.T) {
	d, be := newFake(t)
	be.err = errors.New("no /dev/kvm")
	_, err := d.Create(context.Background(), driver.Spec{ID: id})
	if !errors.Is(err, driver.ErrUnavailable) || !strings.Contains(err.Error(), "/dev/kvm") {
		t.Fatalf("create on a host without the backend: %v", err)
	}
	if len(be.boots) != 0 {
		t.Fatal("booted anyway")
	}
	if _, err := d.Status(context.Background(), id); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("a failed create left an instance: %v", err)
	}
}

func TestCreateRefusesBadSpecs(t *testing.T) {
	d, be := newFake(t)
	for name, spec := range map[string]driver.Spec{
		"no id":         {},
		"hyphen in id":  {ID: "sbx-1"},
		"upper in id":   {ID: "SBX_1"},
		"long id":       {ID: strings.Repeat("a", 65)},
		"unknown tier":  {ID: id, MinTier: "hypervisor"},
		"relative ws":   {ID: id, Workspace: "ws"},
		"unknown key":   {ID: id, Config: map[string]string{"kernel": "/evil"}},
		"too many cpus": {ID: id, Config: map[string]string{"vcpus": "64"}},
		"zero cpus":     {ID: id, Config: map[string]string{"vcpus": "0"}},
		"too much mem":  {ID: id, Config: map[string]string{"memory_mib": "999999"}},
		"tiny mem":      {ID: id, Config: map[string]string{"memory_mib": "16"}},
		"not a number":  {ID: id, Config: map[string]string{"vcpus": "two"}},
	} {
		if _, err := d.Create(context.Background(), spec); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(be.boots) != 0 {
		t.Fatal("a refused spec booted")
	}
}

func TestEveryKnownTierIsMet(t *testing.T) {
	for tier := range knownTiers {
		d, _ := newFake(t)
		if _, err := d.Create(context.Background(), driver.Spec{ID: id, MinTier: tier}); err != nil {
			t.Errorf("min tier %s: %v", tier, err)
		}
	}
}

func TestBootTimeoutKillsTheVM(t *testing.T) {
	d, be := newFake(t)
	be.silent = true
	d.opts.BootTimeoutSeconds = 1
	_, err := d.Create(context.Background(), driver.Spec{ID: id})
	if !errors.Is(err, driver.ErrUnavailable) || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("create with a silent guest: %v", err)
	}
	if killed, _ := be.last().counts(); killed == 0 {
		t.Fatal("the VM was left running")
	}
	if _, err := d.Status(context.Background(), id); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("a failed boot left an instance: %v", err)
	}
}

func TestVMThatExitsIsFailedNotRunning(t *testing.T) {
	d, be := newFake(t)
	if _, err := d.Create(context.Background(), driver.Spec{ID: id}); err != nil {
		t.Fatal(err)
	}
	be.last().exit()
	st, err := d.Status(context.Background(), id)
	if err != nil || st.Phase != driver.PhaseFailed || !strings.Contains(st.Message, "fake console") {
		t.Fatalf("status of an exited VM: %+v %v", st, err)
	}
	if _, _, err := execIn(t, d, driver.ExecRequest{Command: "true"}); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("exec on an exited VM: %v", err)
	}
	if st, err := d.Start(context.Background(), id); err != nil || st.Phase != driver.PhaseRunning {
		t.Fatalf("restart after a crash: %+v %v", st, err)
	}
}

func TestStopWithoutGraceKillsAtOnce(t *testing.T) {
	d, be := newFake(t)
	if _, err := d.Create(context.Background(), driver.Spec{ID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stop(context.Background(), id, 0); err != nil {
		t.Fatal(err)
	}
	if killed, shutdown := be.last().counts(); killed != 1 || shutdown != 0 {
		t.Fatalf("killed %d, shutdown %d", killed, shutdown)
	}
}

func TestStopEndsRunningCommands(t *testing.T) {
	d, _ := newFake(t)
	if _, err := d.Create(context.Background(), driver.Spec{ID: id}); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, _, err := execIn(t, d, driver.ExecRequest{Command: "sleep 30"})
		errCh <- err
	}()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if _, err := d.Stop(context.Background(), id, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err == nil {
		t.Fatal("a command ended by Stop reported an exit code")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("stop waited for the command")
	}
}

func TestExecGuestRefusalIsAnError(t *testing.T) {
	d, _ := newFake(t)
	if _, err := d.Create(context.Background(), driver.Spec{ID: id}); err != nil {
		t.Fatal(err)
	}
	_, _, err := execIn(t, d, driver.ExecRequest{Command: "true", Env: []string{"PATH=/x"}})
	if !errors.Is(err, driver.ErrUnavailable) || !strings.Contains(err.Error(), "PATH") {
		t.Fatalf("replacing PATH: %v", err)
	}
	if _, _, err := execIn(t, d, driver.ExecRequest{}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("empty request: %v", err)
	}
}

func TestAttachWithResize(t *testing.T) {
	d, _ := newFake(t)
	if _, err := d.Create(context.Background(), driver.Spec{ID: id}); err != nil {
		t.Fatal(err)
	}
	in, inw := io.Pipe()
	resize := make(chan driver.WinSize, 1)
	var out syncBuf
	done := make(chan error, 1)
	var res driver.ExecResult
	go func() {
		var err error
		res, err = d.Attach(context.Background(), id, driver.AttachRequest{Size: driver.WinSize{Rows: 10, Cols: 40}, Stdin: in, Stdout: &out, Resize: resize})
		done <- err
	}()
	resize <- driver.WinSize{Rows: 21, Cols: 77}
	time.Sleep(300 * time.Millisecond)
	_, _ = io.WriteString(inw, "stty size; exit 5\n")
	select {
	case err := <-done:
		if err != nil || res.ExitCode != 5 || !strings.Contains(out.String(), "21 77") {
			t.Fatalf("attach: %+v %v %q", res, err, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("attach did not end")
	}
	_ = inw.Close()
}

func TestSlotsRunOut(t *testing.T) {
	d, _ := newFake(t)
	d.opts.Firecracker = &Firecracker{Slots: 1}
	if _, err := d.Create(context.Background(), driver.Spec{ID: "sbx_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(context.Background(), driver.Spec{ID: "sbx_2"}); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("create past the slots: %v", err)
	}
	if err := d.Delete(context.Background(), "sbx_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(context.Background(), driver.Spec{ID: "sbx_2"}); err != nil {
		t.Fatalf("a freed slot was not reused: %v", err)
	}
}

func TestWorkspaceNeedsMkfs(t *testing.T) {
	d, be := newFake(t)
	d.opts.MkfsExt4 = "/no/such/mkfs.ext4"
	_, err := d.Create(context.Background(), driver.Spec{ID: id, Workspace: t.TempDir()})
	if !errors.Is(err, driver.ErrUnavailable) || !strings.Contains(err.Error(), "mkfs.ext4") {
		t.Fatalf("workspace without mkfs: %v", err)
	}
	if len(be.boots) != 0 {
		t.Fatal("booted without the workspace")
	}
}

func TestCloseDeletesEverything(t *testing.T) {
	d, be := newFake(t)
	for _, n := range []string{"sbx_1", "sbx_2"} {
		if _, err := d.Create(context.Background(), driver.Spec{ID: n}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, m := range be.machines {
		if killed, _ := m.counts(); killed == 0 {
			t.Fatal("a VM outlived Close")
		}
	}
}

// The driver behind the out-of-process protocol, as cmd/abhed-microvm serves it.
func TestOverThePluginProtocol(t *testing.T) {
	d, _ := newFake(t)
	sock := filepath.Join(shortTemp(t), "d.sock")
	l, err := plugin.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- plugin.Serve(ctx, l, d) }()
	defer func() { cancel(); <-served }()

	cl, err := plugin.Dial(context.Background(), Name, sock)
	if err != nil {
		t.Fatal(err)
	}
	st, err := cl.Create(context.Background(), driver.Spec{ID: id})
	if err != nil || st.Tier != Tier {
		t.Fatalf("create: %+v %v", st, err)
	}
	var out bytes.Buffer
	res, err := cl.Exec(context.Background(), id, driver.ExecRequest{Argv: []string{"/bin/echo", "over", "the wire"}, Stdout: &out})
	if err != nil || res.ExitCode != 0 || out.String() != "over the wire\n" {
		t.Fatalf("exec: %+v %q %v", res, out.String(), err)
	}
	if err := cl.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestNewRefusesBadOptions(t *testing.T) {
	base := testOptions(t)
	for name, mod := range map[string]func(*Options){
		"no backend":    func(*Options) {},
		"two backends":  func(o *Options) { o.Firecracker, o.Helper = &Firecracker{}, &Helper{} },
		"helper rel":    func(o *Options) { o.Helper = &Helper{Path: "helper"} },
		"kernel rel":    func(o *Options) { o.Helper, o.Kernel = &Helper{}, "vmlinux" },
		"fc no uid":     func(o *Options) { o.Firecracker = &Firecracker{Binary: "/fc", Jailer: "/j"} },
		"fc rel binary": func(o *Options) { o.Firecracker = &Firecracker{Binary: "fc", Jailer: "/j", UIDBase: 1, GIDBase: 1} },
		"fc slots": func(o *Options) {
			o.Firecracker = &Firecracker{Binary: "/fc", Jailer: "/j", UIDBase: 1 << 31, GIDBase: 1}
		},
		"fc cgroup": func(o *Options) {
			o.Firecracker = &Firecracker{Binary: "/fc", Jailer: "/j", UIDBase: 1, GIDBase: 1, ParentCgroup: "../x"}
		},
		"fc seccomp": func(o *Options) {
			o.Firecracker = &Firecracker{Binary: "/fc", Jailer: "/j", UIDBase: 1, GIDBase: 1, SeccompFilter: "f.bpf"}
		},
	} {
		o := base
		mod(&o)
		if _, err := New(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestOneBootAtATime(t *testing.T) {
	d, be := newFake(t)
	if _, err := d.Create(context.Background(), driver.Spec{ID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stop(context.Background(), id, 0); err != nil {
		t.Fatal(err)
	}
	be.delay = 300 * time.Millisecond
	errs := make(chan error, 2)
	for range 2 {
		go func() { _, err := d.Start(context.Background(), id); errs <- err }()
	}
	var conflicts int
	for range 2 {
		if err := <-errs; errors.Is(err, driver.ErrConflict) {
			conflicts++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if conflicts != 1 || len(be.boots) != 2 {
		t.Fatalf("%d conflicts, %d boots: two boots shared one instance", conflicts, len(be.boots))
	}
	if err := d.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceMustBeAFolder(t *testing.T) {
	d, _ := newFake(t)
	f := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(f, nil, 0o600)
	if _, err := d.Create(context.Background(), driver.Spec{ID: id, Workspace: f}); !errors.Is(err, driver.ErrUnavailable) || !strings.Contains(err.Error(), "not a folder") {
		t.Fatalf("a file as the workspace: %v", err)
	}
}
