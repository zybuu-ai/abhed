package container

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/stdiorelay"
)

// fakeRunner records every argv and answers as a healthy engine would,
// unless a test's respond says otherwise.
type fakeRunner struct {
	podman, rootless, selinux bool

	mu    sync.Mutex
	calls [][]string
	// respond, when set and returning ok, answers a call in place of the default.
	respond func(args []string) (out []byte, err error, ok bool)
	// script is what a streamed command runs on the host.
	script string
	// commandFor, when set and returning a command, runs in place of script.
	commandFor func(ctx context.Context, args []string) *exec.Cmd
}

func (f *fakeRunner) record(args []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{}, args...))
}

const healthy = "Seccomp: 2\nNoNewPrivs: 1\nCapEff: 0000000000000000\nCapBnd: 0000000000000000\nUid: 501\nnet Inter-| Receive | Transmit\nnet face |bytes packets\nnet lo: 0 0 0\n"

func (f *fakeRunner) output(_ context.Context, args []string, stdin io.Reader) ([]byte, error) {
	f.record(args)
	if stdin != nil {
		_, _ = io.Copy(io.Discard, stdin)
	}
	if f.respond != nil {
		if out, err, ok := f.respond(args); ok {
			return out, err
		}
	}
	switch args[0] {
	case "--version":
		if f.podman {
			return []byte("podman version 5.2.2\n"), nil
		}
		return []byte("Docker version 28.0.1, build x\n"), nil
	case "info":
		if f.podman {
			return []byte("5.2.2 " + boolText(f.rootless) + " " + boolText(f.selinux) + "\n"), nil
		}
		opts := `["name=seccomp,profile=builtin"`
		if f.rootless {
			opts += `,"name=rootless"`
		}
		if f.selinux {
			opts += `,"name=selinux"`
		}
		return []byte("28.0.1 " + opts + "]\n"), nil
	case "exec":
		if slices.Contains(args, verifyScript) {
			return []byte(strings.Replace(healthy, "Uid: 501", "Uid: "+uidOf(args), 1)), nil
		}
		return nil, nil
	case "container":
		return []byte("running 0 false\n"), nil
	}
	return nil, nil
}

func uidOf(args []string) string {
	for i, a := range args {
		if a == "--user" && i+1 < len(args) {
			u, _, _ := strings.Cut(args[i+1], ":")
			return u
		}
	}
	return ""
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (f *fakeRunner) command(ctx context.Context, args []string) *exec.Cmd {
	f.record(args)
	if f.commandFor != nil {
		if cmd := f.commandFor(ctx, args); cmd != nil {
			return cmd
		}
	}
	script := f.script
	if script == "" {
		script = "exit 0"
	}
	return exec.CommandContext(ctx, "/bin/sh", "-c", script)
}

func (f *fakeRunner) find(verb string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

// runCall is the argv of the container's start.
func (f *fakeRunner) runCall(t *testing.T) []string {
	t.Helper()
	runs := f.find("run")
	if len(runs) == 0 {
		t.Fatal("no container was started")
	}
	return runs[len(runs)-1]
}

func testDriver(t *testing.T, f *fakeRunner, opts Options, pol func(*sandbox.Policy)) (*Driver, string) {
	t.Helper()
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	d := newDriver(Podman, opts, f, func(w string) (sandbox.Policy, error) {
		p := sandbox.DefaultPolicy(w)
		if pol != nil {
			pol(&p)
		}
		return p, nil
	})
	if !f.podman {
		d.name = Docker
	}
	return d, ws
}

func spec(ws string) driver.Spec {
	return driver.Spec{ID: "sbx_0123456789abcdef", Generation: 1, Workspace: ws}
}

func joined(args []string) string { return " " + strings.Join(args, " ") + " " }

// The flags are the boundary; each is asserted with the reason it exists.
func TestRunArgsHoldEveryBoundary(t *testing.T) {
	for _, podman := range []bool{true, false} {
		f := &fakeRunner{podman: podman}
		d, ws := testDriver(t, f, Options{}, func(p *sandbox.Policy) { p.MaxMemoryMB, p.MaxProcs = 1024, 256 })
		if _, err := d.Create(context.Background(), spec(ws)); err != nil {
			t.Fatal(err)
		}
		got := joined(f.runCall(t))
		for _, w := range []struct{ flag, why string }{
			{" run -d --name abhed-sbx-sbx_0123456789abcdef ", "a named, long-lived container"},
			{" --label abhed.sandbox=sbx_0123456789abcdef ", "a restarted server finds its containers by label"},
			{" --init ", "an init reaps what commands leave and passes a stop on"},
			{" --cap-drop ALL ", "a shell needs no Linux capabilities; CAP_SYS_ADMIN is an escape"},
			{" --security-opt no-new-privileges ", "a setuid binary cannot regain what cap-drop removed"},
			{" --read-only ", "the image cannot be changed, so nothing is planted on PATH"},
			{" --tmpfs /tmp:rw,noexec,nosuid,nodev,size=256m ", "scratch space that cannot run what is written there"},
			{" --tmpfs /run:rw,noexec,nosuid,nodev,size=16m ", "the same for /run"},
			{" --network none ", "no network: a stolen secret cannot leave"},
			{" --memory 1024m --memory-swap 1024m ", "the memory bound holds swap too"},
			{" --pids-limit 256 ", "a fork bomb is bounded"},
			{" --cpus 2 ", "one sandbox cannot starve the host"},
			{" --ulimit fsize=536870912:536870912 ", "an unbounded write cannot fill the disk"},
			{" --ulimit core=0:0 ", "no core dumps of what ran inside"},
			{" --ipc private --hostname abhed ", "no shared memory, and the host's name hidden"},
			{" --user 501:20 ", "commands run as this user, never root"},
			{" -v " + ws + ":" + ws + " ", "the workspace, at the same path inside"},
			{" --tmpfs " + ws + "/.abhed:", "Abhed's state hidden"},
			{" -e ABHED_SANDBOX=container ", "commands can tell they are sandboxed"},
			{" --entrypoint /bin/sh docker.io/library/debian:bookworm-slim -c ", "an image's own entrypoint is not run"},
		} {
			if w.flag == " --user 501:20 " {
				w.flag = " --user " + strings.TrimSpace(d.insts["sbx_0123456789abcdef"].user) + " "
			}
			if !strings.Contains(got, w.flag) {
				t.Errorf("podman=%v: missing %q: %s\n%s", podman, w.flag, w.why, got)
			}
		}
		if strings.Contains(got, "nproc=") {
			t.Error("an nproc limit beside --pids-limit")
		}
		if strings.Contains(got, "seccomp=") || strings.Contains(got, "unconfined") || strings.Contains(got, "--privileged") {
			t.Errorf("the default seccomp profile was replaced: %s", got)
		}
		podmanOnly := []string{" --pid private --uts private ", " --read-only-tmpfs=false "}
		for _, flag := range podmanOnly {
			if strings.Contains(got, flag) != podman {
				t.Errorf("podman=%v: %q present=%v", podman, flag, !podman)
			}
		}
	}
}

func TestUserAndUserNamespace(t *testing.T) {
	f := &fakeRunner{podman: true, rootless: true}
	d, ws := testDriver(t, f, Options{}, nil)
	if _, err := d.Create(context.Background(), spec(ws)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joined(f.runCall(t)), " --userns keep-id ") {
		t.Error("rootless podman without keep-id maps this user to root inside")
	}
	f = &fakeRunner{podman: true}
	d, ws = testDriver(t, f, Options{}, nil)
	if _, err := d.Create(context.Background(), spec(ws)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(joined(f.runCall(t)), "--userns") {
		t.Error("keep-id on rootful podman")
	}
	for _, c := range []struct {
		uid, wsUID int
		eng        engineInfo
		want       int
	}{
		{501, 501, engineInfo{}, 501},
		{0, 1000, engineInfo{}, 1000},
		{0, 0, engineInfo{}, 65534},
		{501, 501, engineInfo{rootless: true}, 0},
		{501, 501, engineInfo{rootless: true, podman: true}, 501},
	} {
		if got, _ := userFor(c.uid, 20, c.wsUID, 20, c.eng); got != c.want {
			t.Errorf("userFor(%d, ws %d, %+v) = %d, want %d", c.uid, c.wsUID, c.eng, got, c.want)
		}
	}
}

func TestNetworkFollowsThePolicy(t *testing.T) {
	f := &fakeRunner{podman: true}
	d, ws := testDriver(t, f, Options{}, func(p *sandbox.Policy) { p.AllowNetwork = true })
	if _, err := d.Create(context.Background(), spec(ws)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(joined(f.runCall(t)), "--network none") {
		t.Error("allow_network should leave the engine's network")
	}
}

func TestSpecMayOnlyNarrowResources(t *testing.T) {
	bound := func(p *sandbox.Policy) { p.MaxMemoryMB, p.MaxProcs = 512, 100 }
	for _, c := range []struct {
		cfg     map[string]string
		want    string
		refused bool
	}{
		{map[string]string{"memory_mb": "128", "pids": "50", "cpus": "0.5"}, " --memory 128m --memory-swap 128m --pids-limit 50 ", false},
		{map[string]string{"cpus": "0.5"}, " --cpus 0.5 ", false},
		{map[string]string{"memory_mb": "4096"}, "", true},
		{map[string]string{"pids": "1000"}, "", true},
		{map[string]string{"cpus": "8"}, "", true},
		{map[string]string{"memory_mb": "-1"}, "", true},
		{map[string]string{"privileged": "true"}, "", true},
	} {
		f := &fakeRunner{podman: true}
		d, ws := testDriver(t, f, Options{}, bound)
		s := spec(ws)
		s.Config = c.cfg
		_, err := d.Create(context.Background(), s)
		if c.refused {
			if !errors.Is(err, driver.ErrInvalid) || len(f.find("run")) != 0 {
				t.Errorf("%v: got %v, want refused before any container", c.cfg, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%v: %v", c.cfg, err)
		}
		if got := joined(f.runCall(t)); !strings.Contains(got, c.want) {
			t.Errorf("%v: missing %q in %s", c.cfg, c.want, got)
		}
	}
}

func TestSELinuxRelabelsOnlyTheWorkspace(t *testing.T) {
	ro := t.TempDir()
	extra, shared := t.TempDir(), t.TempDir()
	f := &fakeRunner{podman: true, selinux: true}
	d, ws := testDriver(t, f, Options{Mounts: []Mount{{Host: extra, Target: "/extra"}, {Host: shared, Target: "/shared", Relabel: true}}},
		func(p *sandbox.Policy) {
			p.ReadOnlyPaths = []string{ro}
		})
	if err := os.WriteFile(filepath.Join(ws, "settings.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.policy = func(w string) (sandbox.Policy, error) {
		p := sandbox.DefaultPolicy(w)
		p.ReadOnlyPaths = []string{ro}
		p.WriteProtected = []string{filepath.Join(w, "settings.json")}
		return p, nil
	}
	if _, err := d.Create(context.Background(), spec(ws)); err != nil {
		t.Fatal(err)
	}
	got := joined(f.runCall(t))
	realExtra, _ := filepath.EvalSymlinks(extra)
	realShared, _ := filepath.EvalSymlinks(shared)
	for _, want := range []string{
		" -v " + ws + ":" + ws + ":z ",
		" -v " + ro + ":" + ro + ":ro ",
		" -v " + filepath.Join(ws, "settings.json") + ":" + filepath.Join(ws, "settings.json") + ":ro,z ",
		" -v " + realExtra + ":/extra:ro ",
		" -v " + realShared + ":/shared:ro,z ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

func TestMountAdmission(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home folder")
	}
	root := t.TempDir()
	realRoot, _ := filepath.EvalSymlinks(root)
	inside := filepath.Join(realRoot, "data")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(realRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	// A short folder: a socket's path has a length limit.
	sockDir, err := os.MkdirTemp("/tmp", "abhed-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "engine.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	opts := Options{MountRoots: []MountRoot{{Path: root}}}
	cases := []struct {
		name   string
		mounts string
		ok     bool
	}{
		{"a folder under a root, read-only", `[{"host":"` + inside + `","target":"/data"}]`, true},
		{"writable under a read-only root", `[{"host":"` + inside + `","target":"/data","rw":true}]`, false},
		{"a link inside the root that leads out", `[{"host":"` + filepath.Join(realRoot, "escape") + `","target":"/data"}]`, false},
		{"not under any root", `[{"host":"` + outside + `","target":"/data"}]`, false},
		{"/proc", `[{"host":"/proc","target":"/data"}]`, false},
		{"/sys", `[{"host":"/sys","target":"/data"}]`, false},
		{"/dev", `[{"host":"/dev","target":"/data"}]`, false},
		{"relative", `[{"host":"data","target":"/data"}]`, false},
		{"target over /proc", `[{"host":"` + inside + `","target":"/proc/x"}]`, false},
		{"target over /tmp", `[{"host":"` + inside + `","target":"/tmp"}]`, false},
		{"target is the root", `[{"host":"` + inside + `","target":"/"}]`, false},
		{"a colon in the target", `[{"host":"` + inside + `","target":"/a:b"}]`, false},
		{"an unknown field", `[{"host":"` + inside + `","target":"/data","privileged":true}]`, false},
	}
	for _, c := range cases {
		f := &fakeRunner{podman: true}
		d, ws := testDriver(t, f, opts, nil)
		s := spec(ws)
		s.Config = map[string]string{"mounts": c.mounts}
		_, err := d.Create(context.Background(), s)
		if c.ok != (err == nil) {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
			continue
		}
		if c.ok && !strings.Contains(joined(f.runCall(t)), " -v "+inside+":/data:ro ") {
			t.Errorf("%s: not mounted read-only: %s", c.name, joined(f.runCall(t)))
		}
		if !c.ok && len(f.find("run")) != 0 {
			t.Errorf("%s: a container was started", c.name)
		}
	}

	// Mounts the operator configures pass the same refusals.
	ws := t.TempDir()
	for name, m := range map[string]Mount{
		"Abhed's home state":                                    {Host: filepath.Join(home, ".abhed"), Target: "/x"},
		"the home folder whole":                                 {Host: home, Target: "/x"},
		"a folder holding home":                                 {Host: filepath.Dir(home), Target: "/x"},
		"the workspace's state":                                 {Host: filepath.Join(ws, ".abhed"), Target: "/x"},
		"the folder above the workspace, which holds its state": {Host: filepath.Dir(ws), Target: "/x"},
		"an engine socket":                                      {Host: sock, Target: "/x"},
		"/run":                                                  {Host: "/run", Target: "/x"},
		"/var/run":                                              {Host: "/var/run", Target: "/x"},
		"/":                                                     {Host: "/", Target: "/x"},
		"over the workspace":                                    {Host: inside, Target: ws},
		"inside the workspace":                                  {Host: inside, Target: filepath.Join(ws, "sub")},
	} {
		_ = os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700)
		_ = os.MkdirAll(filepath.Join(home, ".abhed"), 0o700)
		pol := sandbox.DefaultPolicy(ws)
		_, err := Options{Mounts: []Mount{m}}.admit(driver.Spec{}, pol, denied(pol))
		if !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	// A configured root that is itself refused never takes effect.
	if err := (Options{MountRoots: []MountRoot{{Path: home}}}).check(); err == nil {
		t.Error("the whole home folder was accepted as a mount root")
	}
}

func TestGPUsAreOptIn(t *testing.T) {
	for _, c := range []struct {
		allowed []string
		asked   string
		ok      bool
	}{
		{nil, "nvidia.com/gpu=all", false},
		{[]string{"nvidia.com/gpu=0"}, "nvidia.com/gpu=all", false},
		{[]string{"nvidia.com/gpu=all"}, "/dev/nvidia0", false},
		{[]string{"nvidia.com/gpu=all"}, "nvidia.com/gpu=all", true},
	} {
		f := &fakeRunner{podman: true}
		d, ws := testDriver(t, f, Options{GPUs: c.allowed}, nil)
		s := spec(ws)
		s.Config = map[string]string{"gpus": c.asked}
		_, err := d.Create(context.Background(), s)
		if c.ok != (err == nil) {
			t.Errorf("%v asking %s: %v", c.allowed, c.asked, err)
			continue
		}
		if c.ok && !strings.Contains(joined(f.runCall(t)), " --device nvidia.com/gpu=all ") {
			t.Error("the CDI device was not passed")
		}
	}
	f := &fakeRunner{podman: true}
	d, ws := testDriver(t, f, Options{}, nil)
	if _, err := d.Create(context.Background(), spec(ws)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(joined(f.runCall(t)), "--device") || strings.Contains(joined(f.runCall(t)), "--gpus") {
		t.Error("a device without asking")
	}
	if err := (Options{GPUs: []string{"/dev/nvidia0"}}).check(); err == nil {
		t.Error("a raw device accepted as a GPU")
	}
}

func TestImages(t *testing.T) {
	t.Setenv("ABHED_SANDBOX_IMAGE", "")
	for _, c := range []struct {
		opts  Options
		asked string
		want  string
		ok    bool
	}{
		{Options{}, "", sandbox.Image, true},
		{Options{Image: "registry.local/base:1"}, "", "registry.local/base:1", true},
		{Options{Images: []string{"registry.local/py:3"}}, "registry.local/py:3", "registry.local/py:3", true},
		{Options{}, "attacker/image:latest", "", false},
		{Options{Images: []string{"--privileged"}}, "", "", false},
	} {
		if err := c.opts.check(); err != nil {
			if c.ok {
				t.Errorf("%+v: %v", c.opts, err)
			}
			continue
		}
		f := &fakeRunner{podman: true}
		d, ws := testDriver(t, f, c.opts, nil)
		s := spec(ws)
		if c.asked != "" {
			s.Config = map[string]string{"image": c.asked}
		}
		_, err := d.Create(context.Background(), s)
		if c.ok != (err == nil) {
			t.Errorf("%+v asking %q: %v", c.opts, c.asked, err)
			continue
		}
		if c.ok && !strings.Contains(joined(f.runCall(t)), " "+c.want+" -c ") {
			t.Errorf("image %s not run: %s", c.want, joined(f.runCall(t)))
		}
	}
}

func TestMinTierAndChosenTier(t *testing.T) {
	f := &fakeRunner{podman: true}
	d, ws := testDriver(t, f, Options{}, nil)
	for tier, want := range map[string]error{"vm": driver.ErrUnavailable, "microvm": driver.ErrUnavailable, "bogus": driver.ErrInvalid} {
		s := spec(ws)
		s.MinTier = tier
		if _, err := d.Create(context.Background(), s); !errors.Is(err, want) {
			t.Errorf("min tier %s: %v, want %v", tier, err, want)
		}
	}
	d.policy = func(w string) (sandbox.Policy, error) {
		p := sandbox.DefaultPolicy(w)
		p.MinTier = sandbox.TierVM
		return p, nil
	}
	if _, err := d.Create(context.Background(), spec(ws)); !errors.Is(err, driver.ErrUnavailable) {
		t.Errorf("sandbox.min_tier vm: %v", err)
	}
	d.policy = func(w string) (sandbox.Policy, error) {
		p := sandbox.DefaultPolicy(w)
		p.Tier = sandbox.TierFence
		return p, nil
	}
	if _, err := d.Create(context.Background(), spec(ws)); !errors.Is(err, driver.ErrUnavailable) {
		t.Errorf("sandbox.tier fence: %v", err)
	}
	if len(f.find("run")) != 0 {
		t.Error("a container was started for a tier it cannot meet")
	}
	d.policy = func(w string) (sandbox.Policy, error) { return sandbox.DefaultPolicy(w), nil }
	st, err := d.Create(context.Background(), driver.Spec{ID: "sbx_ok2", Workspace: ws, MinTier: "container"})
	if err != nil || st.Tier != "container" {
		t.Fatalf("min tier container: %+v %v", st, err)
	}
}

// A container whose kernel says it is weaker than its arguments is removed.
func TestVerifyRefusesAWeakerContainer(t *testing.T) {
	for name, report := range map[string]string{
		"no seccomp":        strings.Replace(healthy, "Seccomp: 2", "Seccomp: 0", 1),
		"no no_new_privs":   strings.Replace(healthy, "NoNewPrivs: 1", "NoNewPrivs: 0", 1),
		"a capability":      strings.Replace(healthy, "CapEff: 0000000000000000", "CapEff: 00000000a80425fb", 1),
		"a bounding set":    strings.Replace(healthy, "CapBnd: 0000000000000000", "CapBnd: 00000000a80425fb", 1),
		"root":              strings.Replace(healthy, "Uid: 501", "Uid: 0", 1),
		"a network device":  healthy + "net eth0: 1 2 3\n",
		"a writable rootfs": healthy + "rootfs writable\n",
		"nothing at all":    "",
	} {
		f := &fakeRunner{podman: true}
		f.respond = func(args []string) ([]byte, error, bool) {
			if args[0] == "exec" && slices.Contains(args, verifyScript) {
				return []byte(strings.Replace(report, "Uid: 501", "Uid: "+uidOf(args), 1)), nil, true
			}
			return nil, nil, false
		}
		d, ws := testDriver(t, f, Options{}, nil)
		_, err := d.Create(context.Background(), spec(ws))
		if !errors.Is(err, driver.ErrUnavailable) {
			t.Errorf("%s: %v, want unavailable", name, err)
		}
		rms := f.find("rm")
		if len(rms) == 0 || !slices.Contains(rms[len(rms)-1], "abhed-sbx-sbx_0123456789abcdef") {
			t.Errorf("%s: the weaker container was not removed", name)
		}
		if _, err := d.Status(context.Background(), "sbx_0123456789abcdef"); err == nil {
			f.respond = func([]string) ([]byte, error, bool) {
				return nil, &engineError{stderr: "no such container"}, true
			}
			if _, err := d.Status(context.Background(), "sbx_0123456789abcdef"); !errors.Is(err, driver.ErrNotFound) {
				t.Errorf("%s: the instance was kept: %v", name, err)
			}
		}
	}
}

func TestEngineThatDoesNotAnswerIsUnavailable(t *testing.T) {
	f := &fakeRunner{podman: true}
	f.respond = func(args []string) ([]byte, error, bool) {
		if args[0] == "info" {
			return nil, &engineError{args: args, stderr: "Cannot connect to Podman"}, true
		}
		return nil, nil, false
	}
	d, ws := testDriver(t, f, Options{}, nil)
	if _, err := d.Create(context.Background(), spec(ws)); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	f2 := &fakeRunner{podman: false}
	d2 := newDriver(Podman, Options{}, f2, d.policy)
	if _, err := d2.Create(context.Background(), spec(ws)); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("docker behind the podman driver: %v", err)
	}
}

func TestExecArgvAndEnvironment(t *testing.T) {
	f := &fakeRunner{podman: true, script: "exit 4"}
	d, ws := testDriver(t, f, Options{}, nil)
	if _, err := d.Create(context.Background(), spec(ws)); err != nil {
		t.Fatal(err)
	}
	id := "sbx_0123456789abcdef"
	res, err := d.Exec(context.Background(), id, driver.ExecRequest{Argv: []string{"echo", "a b"}, Cwd: "sub", Env: []string{"TOKEN=s3cret"}})
	if err != nil || res.ExitCode != 4 {
		t.Fatalf("exec: %+v %v", res, err)
	}
	var execCall []string
	for _, c := range f.find("exec") {
		if !slices.Contains(c, verifyScript) {
			execCall = c
		}
	}
	got := joined(execCall)
	for _, want := range []string{" exec -i --user ", " -w " + filepath.Join(ws, "sub") + " ", " -e TOKEN abhed-sbx-" + id + " /bin/sh -c ", " echo 'a b' "} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "s3cret") {
		t.Error("an environment value appeared in the engine's arguments")
	}
	for _, env := range [][]string{{"PATH=/evil"}, {"HOME=/"}, {"https_proxy=http://x"}, {"LD_PRELOAD=/x.so"}, {"novalue"}} {
		if _, err := d.Exec(context.Background(), id, driver.ExecRequest{Command: "true", Env: env}); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("env %v: %v", env, err)
		}
	}
	// A bare entry may be a secret's value: the refusal gives its position only.
	if _, err := d.Exec(context.Background(), id, driver.ExecRequest{Command: "true", Env: []string{"A=1", "ghp_secretvalue"}}); err == nil ||
		strings.Contains(err.Error(), "ghp_") || !strings.Contains(err.Error(), "entry 2") {
		t.Errorf("a bare entry: %v", err)
	}
	for _, dir := range []string{"..", "/etc", ws + "/../x"} {
		if _, err := d.Exec(context.Background(), id, driver.ExecRequest{Command: "true", Cwd: dir}); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("cwd %s: %v", dir, err)
		}
	}

	// Podman's own failure is not the command's exit code.
	f.script = "exit 125"
	if _, err := d.Exec(context.Background(), id, driver.ExecRequest{Command: "true"}); !errors.Is(err, driver.ErrUnavailable) {
		t.Errorf("exit 125 from podman: %v", err)
	}
	// Nor is a failure when the container has stopped around the command.
	f.script = "exit 1"
	f.respond = func(args []string) ([]byte, error, bool) {
		if args[0] == "container" {
			return []byte("exited 137 false\n"), nil, true
		}
		return nil, nil, false
	}
	if _, err := d.Exec(context.Background(), id, driver.ExecRequest{Command: "true"}); !errors.Is(err, driver.ErrUnavailable) {
		t.Errorf("a command whose container stopped: %v", err)
	}
	f.respond = nil

	if _, err := d.Stop(context.Background(), id, 0); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.find("stop")[0], "0") {
		t.Errorf("stop: %v", f.find("stop"))
	}
	if _, err := d.Exec(context.Background(), id, driver.ExecRequest{Command: "true"}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("exec on a stopped instance: %v", err)
	}
	if err := d.CopyIn(context.Background(), id, ws, strings.NewReader("")); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("copy on a stopped instance: %v", err)
	}
}

// A cancelled command's processes inside the container are ended.
func TestCancelKillsInsideTheContainer(t *testing.T) {
	f := &fakeRunner{podman: true, script: "sleep 30"}
	d, ws := testDriver(t, f, Options{}, nil)
	if _, err := d.Create(context.Background(), spec(ws)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := d.Exec(ctx, "sbx_0123456789abcdef", driver.ExecRequest{Command: "sleep 300"})
		done <- err
	}()
	for len(f.find("exec")) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled command reported success")
	}
	var kill []string
	for _, c := range f.find("exec") {
		if slices.Contains(c, killTree) {
			kill = c
		}
	}
	if kill == nil || !strings.HasPrefix(kill[len(kill)-1], "/tmp/.abhed-exec-") {
		t.Fatalf("no kill inside the container: %v", kill)
	}
}

// With git protection, a repository made after the container started is
// protected by replacing the idle container with one that mounts it; while
// the container is in use, the command is refused instead.
func TestNewGitFolderRestartsAnIdleContainer(t *testing.T) {
	f := &fakeRunner{podman: true}
	var mu sync.Mutex
	var events []map[string]any
	d, ws := testDriver(t, f, Options{Record: func(ev string, p map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		if ev == EventRestarted {
			events = append(events, p)
		}
		return nil
	}}, func(p *sandbox.Policy) { p.ProtectGit = true })
	ctx := context.Background()
	if _, err := d.Create(ctx, spec(ws)); err != nil {
		t.Fatal(err)
	}
	id := "sbx_0123456789abcdef"
	if _, err := d.Exec(ctx, id, driver.ExecRequest{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	newRepo := func(name string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(ws, name, ".git", "hooks"), 0o755); err != nil {
			t.Fatal(err)
		}
		for file, text := range map[string]string{"config": "[core]\n", "HEAD": "ref: refs/heads/main\n"} {
			if err := os.WriteFile(filepath.Join(ws, name, ".git", file), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return filepath.Join(ws, name, ".git", "config") + ":ro"
	}
	// A handed-out command keeps the container in use until its context ends.
	cctx, cancel := context.WithCancel(ctx)
	if _, err := driver.Command(cctx, d, id, driver.CommandRequest{Command: "sleep 1"}); err != nil {
		t.Fatal(err)
	}
	runs := len(f.find("run"))
	mount := newRepo("repo")
	_, err := d.Exec(ctx, id, driver.ExecRequest{Command: "true"})
	if !errors.Is(err, driver.ErrConflict) || !strings.Contains(err.Error(), "in use") || len(f.find("run")) != runs {
		t.Fatalf("a command while the container is in use: %v", err)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err = d.Exec(ctx, id, driver.ExecRequest{Command: "true"})
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("a command once the container is idle: %v", err)
	}
	if len(f.find("run")) != runs+1 || !strings.Contains(joined(f.runCall(t)), mount) {
		t.Errorf("the restart did not protect the new repository: %s", joined(f.runCall(t)))
	}
	mu.Lock()
	if len(events) != 1 || events[0]["outcome"] != "restarted" || events[0]["generation"] != int64(1) {
		t.Errorf("restart record %v", events)
	}
	mu.Unlock()
	if st, err := d.Status(ctx, id); err != nil || st.Phase != driver.PhaseRunning {
		t.Fatalf("status after the restart: %+v %v", st, err)
	}
}

func TestDeleteOnlyRemovesThisDriversContainer(t *testing.T) {
	f := &fakeRunner{podman: true}
	d, _ := testDriver(t, f, Options{}, nil)
	f.respond = func(args []string) ([]byte, error, bool) {
		if args[0] == "container" {
			return []byte("sbx_other docker\n"), nil, true
		}
		return nil, nil, false
	}
	if err := d.Delete(context.Background(), "sbx_x"); !errors.Is(err, driver.ErrNotFound) || len(f.find("rm")) != 0 {
		t.Fatalf("someone else's container: %v, rm %v", err, f.find("rm"))
	}
	f.respond = func(args []string) ([]byte, error, bool) {
		if args[0] == "container" {
			return []byte("sbx_x podman\n"), nil, true
		}
		return nil, nil, false
	}
	if err := d.Delete(context.Background(), "sbx_x"); err != nil || len(f.find("rm")) != 1 {
		t.Fatalf("a leftover of this driver: %v, rm %v", err, f.find("rm"))
	}
}

func TestCreateValidatesAndIsIdempotent(t *testing.T) {
	f := &fakeRunner{podman: true}
	d, ws := testDriver(t, f, Options{}, nil)
	for _, s := range []driver.Spec{
		{ID: "", Workspace: ws},
		{ID: "a/b", Workspace: ws},
		{ID: "-rm", Workspace: ws},
		{ID: "ok", Workspace: "relative"},
		{ID: "ok", Workspace: ws + "/../x"},
		{ID: "ok", Workspace: "/a:b"},
	} {
		if _, err := d.Create(context.Background(), s); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("%+v: %v", s, err)
		}
	}
	if _, err := d.Create(context.Background(), spec(ws)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(context.Background(), spec(ws)); !errors.Is(err, driver.ErrExists) {
		t.Errorf("a second create: %v", err)
	}
	if err := driver.Check(d); err != nil {
		t.Errorf("capabilities: %v", err)
	}
	if info := d.Info(); !info.Has(driver.CapAttach) || !info.Has(driver.CapCopy) || !info.Has(driver.CapForward) || info.Has(driver.CapSnapshot) {
		t.Errorf("capabilities %v", info.Capabilities)
	}
	// A leftover container of the same name is an existing instance.
	f2 := &fakeRunner{podman: true}
	f2.respond = func(args []string) ([]byte, error, bool) {
		if args[0] == "run" {
			return nil, &engineError{args: args, stderr: `the container name "abhed-sbx-x" is already in use`}, true
		}
		return nil, nil, false
	}
	d2, ws2 := testDriver(t, f2, Options{}, nil)
	if _, err := d2.Create(context.Background(), spec(ws2)); !errors.Is(err, driver.ErrExists) {
		t.Errorf("a name in use: %v", err)
	}
}

func TestEgressAllowlist(t *testing.T) {
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "example.com", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	withAllowlist := func(p *sandbox.Policy) { p.Egress = pol }

	// Off Linux the engine is in a VM: refused, not run with no proxy.
	old := hostOS
	t.Cleanup(func() { hostOS = old })
	hostOS = "darwin"
	f := &fakeRunner{podman: true}
	d, ws := testDriver(t, f, Options{}, withAllowlist)
	if _, err := d.Create(context.Background(), spec(ws)); !errors.Is(err, driver.ErrUnavailable) || len(f.find("run")) != 0 {
		t.Fatalf("allowlist with the engine in a VM: %v", err)
	}
	hostOS = "linux"
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.1:2375")
	f = &fakeRunner{}
	d, ws = testDriver(t, f, Options{}, withAllowlist)
	if _, err := d.Create(context.Background(), spec(ws)); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("allowlist with a remote engine: %v", err)
	}
	t.Setenv("DOCKER_HOST", "")

	// On Linux: no network, the relay is the entrypoint, and each command
	// gets its own credential, forwarded by name.
	exe := filepath.Join(t.TempDir(), "abhed")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExe := relayExecutable
	t.Cleanup(func() { relayExecutable = oldExe })
	relayExecutable = func() (string, error) { return exe, nil }
	var mu sync.Mutex
	var recorded []map[string]any
	f = &fakeRunner{podman: true, selinux: true, script: `test -n "$HTTPS_PROXY" && echo "$HTTPS_PROXY"`}
	d, ws = testDriver(t, f, Options{Record: func(ev string, p map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		recorded = append(recorded, p)
		return nil
	}}, withAllowlist)
	if _, err := d.Create(context.Background(), spec(ws)); err != nil {
		t.Fatal(err)
	}
	in := d.insts["sbx_0123456789abcdef"]
	got := joined(f.runCall(t))
	for _, want := range []string{
		" --network none ",
		" -v " + in.egress.dir + ":/run/abhed-egress:ro,Z ",
		" --entrypoint /run/abhed-egress/relay ",
		" " + egress.RelayArg + " /run/abhed-egress/proxy.sock " + in.egress.listen + " -- /bin/sh -c ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if b, err := os.ReadFile(filepath.Join(in.egress.dir, "relay")); err != nil || string(b) != "#!/bin/sh\n" {
		t.Errorf("the relay was not placed: %v", err)
	}
	var out bytes.Buffer
	if _, err := d.Exec(context.Background(), "sbx_0123456789abcdef", driver.ExecRequest{Command: "true", Stdout: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "@"+in.egress.listen) {
		t.Errorf("the command had no proxy credential: %q", out.String())
	}
	var execCall []string
	for _, c := range f.find("exec") {
		if !slices.Contains(c, verifyScript) {
			execCall = c
		}
	}
	if s := joined(execCall); !strings.Contains(s, " -e HTTPS_PROXY ") || strings.Contains(s, "http://") {
		t.Errorf("the credential was not forwarded by name only: %s", s)
	}
	dir := in.egress.dir
	if err := d.Delete(context.Background(), "sbx_0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the egress folder outlived the sandbox: %v", err)
	}
}

// Where the engine is in a VM, the operator's Linux build is bound in as the
// relay and run over a long-lived exec; the sandbox is ready once it listens,
// and the exec ends with the sandbox.
func TestEgressAllowlistThroughAnExecWhereTheEngineIsInAVM(t *testing.T) {
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "example.com", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	old := hostOS
	t.Cleanup(func() { hostOS = old })
	hostOS = "darwin"
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var relays atomic.Int32
	f := &fakeRunner{podman: true}
	// This test binary is the relay: its init runs it for the relay's argument.
	f.commandFor = func(ctx context.Context, args []string) *exec.Cmd {
		if len(args) < 2 || args[len(args)-2] != stdiorelay.Arg {
			return nil
		}
		relays.Add(1)
		return exec.CommandContext(ctx, exe, stdiorelay.Arg, "127.0.0.1:0")
	}
	d, ws := testDriver(t, f, Options{RelayBinary: "/opt/abhed/abhed-linux-arm64"}, func(p *sandbox.Policy) { p.Egress = pol })
	if _, err := d.Create(context.Background(), spec(ws)); err != nil {
		t.Fatal(err)
	}
	in := d.insts["sbx_0123456789abcdef"]
	got := joined(f.runCall(t))
	for _, want := range []string{" --network none ", " -v /opt/abhed/abhed-linux-arm64:/run/abhed-egress/relay:ro ",
		" --entrypoint /bin/sh "} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	var relayCall []string
	for _, c := range f.find("exec") {
		if slices.Contains(c, stdiorelay.Arg) {
			relayCall = c
		}
	}
	if want := " exec -i --user " + in.user + " " + in.name + " /run/abhed-egress/relay " + stdiorelay.Arg + " " + in.egress.listen + " "; joined(relayCall) != want {
		t.Errorf("relay exec %q, want %q", joined(relayCall), want)
	}
	if relays.Load() != 1 {
		t.Fatalf("the relay ran %d times", relays.Load())
	}
	done := in.egress.relayDone
	if err := d.Delete(context.Background(), "sbx_0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay's exec outlived the sandbox")
	}
	// A relay that never listens refuses the sandbox.
	f2 := &fakeRunner{podman: true}
	f2.commandFor = func(ctx context.Context, args []string) *exec.Cmd {
		if len(args) < 2 || args[len(args)-2] != stdiorelay.Arg {
			return nil
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", "echo 'exec format error' >&2; exit 126")
	}
	d2, ws2 := testDriver(t, f2, Options{RelayBinary: "/opt/abhed/abhed-linux-arm64"}, func(p *sandbox.Policy) { p.Egress = pol })
	if _, err := d2.Create(context.Background(), spec(ws2)); !errors.Is(err, driver.ErrUnavailable) || !strings.Contains(err.Error(), "exec format error") {
		t.Fatalf("a relay that never listened: %v", err)
	}
}

func TestArchiveRecheck(t *testing.T) {
	type entry struct {
		name, link string
		flag       byte
		mode       int64
		body       string
	}
	build := func(es ...entry) *bytes.Buffer {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		for _, e := range es {
			h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.flag, Mode: e.mode, Size: int64(len(e.body)), Uid: 0, Gid: 0}
			if e.flag != tar.TypeReg {
				h.Size = 0
			}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if e.flag == tar.TypeReg {
				_, _ = tw.Write([]byte(e.body))
			}
		}
		_ = tw.Close()
		return &b
	}
	for name, es := range map[string][]entry{
		"an absolute name":          {{name: "/etc/passwd", flag: tar.TypeReg}},
		"a climb out":               {{name: "../x", flag: tar.TypeReg}},
		"a climb in the middle":     {{name: "a/../../x", flag: tar.TypeReg}},
		"an absolute link":          {{name: "l", link: "/etc", flag: tar.TypeSymlink}},
		"a relative link out":       {{name: "a/l", link: "../../etc", flag: tar.TypeSymlink}},
		"a hard link out":           {{name: "h", link: "../etc/passwd", flag: tar.TypeLink}},
		"a device":                  {{name: "d", flag: tar.TypeChar}},
		"a pipe":                    {{name: "p", flag: tar.TypeFifo}},
		"a backslash":               {{name: `a\..\..\x`, flag: tar.TypeReg}},
		"good then bad stops there": {{name: "ok", flag: tar.TypeReg, body: "x"}, {name: "/bad", flag: tar.TypeReg}},
	} {
		if err := recheck(build(es...), io.Discard, 501, 20); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	var out bytes.Buffer
	err := recheck(build(
		entry{name: "dir/", flag: tar.TypeDir, mode: 0o755},
		entry{name: "dir/run", flag: tar.TypeReg, mode: 0o6755, body: "hello"},
		entry{name: "dir/l", link: "run", flag: tar.TypeSymlink, mode: 0o777},
	), &out, 501, 20)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Uid != 501 || h.Gid != 20 {
			t.Errorf("%s owned by %d:%d", h.Name, h.Uid, h.Gid)
		}
		if h.Mode&0o7000 != 0 {
			t.Errorf("%s kept mode %o", h.Name, h.Mode)
		}
		if h.Name == "dir/run" {
			if b, _ := io.ReadAll(tr); string(b) != "hello" {
				t.Errorf("content %q", b)
			}
		}
	}
}

func TestCopyPathsStayInTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	outside := t.TempDir()
	_ = os.MkdirAll(filepath.Join(ws, "sub"), 0o755)
	_ = os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700)
	_ = os.Symlink(outside, filepath.Join(ws, "out"))
	_ = os.Symlink(filepath.Join(ws, ".abhed"), filepath.Join(ws, "state"))
	if got, err := copyPath(ws, "sub"); err != nil || got != filepath.Join(ws, "sub") {
		t.Errorf("sub: %q %v", got, err)
	}
	for _, p := range []string{"out", "state", ".abhed", ".ABHED", "/etc", "../x", "missing"} {
		if _, err := copyPath(ws, p); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("%s: %v, want refused", p, err)
		}
	}
}

// A mount root may not overlap where sandbox workspaces or snapshots are
// kept: a mount there would hand one sandbox the others' files.
func TestMountRootsStayClearOfReservedFolders(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	for name, o := range map[string]Options{
		"workspaces inside the root": {MountRoots: []MountRoot{{Path: root}}, Reserved: []string{filepath.Join(root, "sandboxes")}},
		"root inside the workspaces": {MountRoots: []MountRoot{{Path: filepath.Join(root, "x")}}, Reserved: []string{root}},
		"snapshots inside the root":  {MountRoots: []MountRoot{{Path: root}}, SnapshotDir: filepath.Join(root, "snaps")},
	} {
		_ = os.MkdirAll(filepath.Join(root, "x"), 0o700)
		if err := o.check(); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	ok := Options{MountRoots: []MountRoot{{Path: root}}, Reserved: []string{other}, SnapshotDir: filepath.Join(other, "snaps")}
	if err := ok.check(); err != nil {
		t.Fatalf("a root clear of both: %v", err)
	}
}
