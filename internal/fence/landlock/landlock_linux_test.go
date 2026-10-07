//go:build linux

package landlock

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The test binary re-executes itself in two roles. As the launcher it builds
// the ruleset from the spec in its environment, restricts its thread and
// execs itself again; as the probe, now confined, it performs one operation
// and reports whether the kernel allowed it.
const (
	roleEnv    = "ABHED_FENCE_LANDLOCK_ROLE"
	specEnv    = "ABHED_FENCE_LANDLOCK_SPEC"
	requireEnv = "ABHED_REQUIRE_FENCE"
)

func TestMain(m *testing.M) {
	switch os.Getenv(roleEnv) {
	case "launch":
		fmt.Fprintln(os.Stderr, launch())
		os.Exit(3)
	case "probe":
		os.Exit(probe(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// launch confines itself and execs the probe; it returns only on failure.
func launch() error {
	var s Spec
	if err := json.Unmarshal([]byte(os.Getenv(specEnv)), &s); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	if s.Exec != nil || s.Write != nil || s.Deny != nil {
		abi, err := Detect()
		if err != nil {
			return err
		}
		r, err := New(s, abi)
		if err != nil {
			return err
		}
		if err := r.Restrict(); err != nil {
			return err
		}
	}
	// unix.Exec keeps a duplicate variable's first value, so replace the role.
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, roleEnv+"=") {
			env = append(env, kv)
		}
	}
	env = append(env, roleEnv+"=probe")
	return unix.Exec(os.Args[0], os.Args, env) // #nosec G204 -- the test binary itself
}

// probe exits 0 when the operation succeeded, 1 when the kernel refused it
// with EACCES or EPERM, and 2 on any other failure.
func probe(args []string) int {
	if len(args) != 2 {
		return 2
	}
	var err error
	switch op, arg := args[0], args[1]; op {
	case "read":
		_, err = os.ReadFile(arg) // #nosec G304 -- a path the test chose
	case "write":
		err = os.WriteFile(arg, []byte("x"), 0o600)
	case "connect":
		var c net.Conn
		if c, err = net.Dial("tcp", arg); err == nil {
			_ = c.Close()
		}
	case "bind":
		var l net.Listener
		if l, err = net.Listen("tcp", arg); err == nil {
			_ = l.Close()
		}
	default:
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return 1
	}
	fmt.Fprintln(os.Stderr, err)
	return 2
}

// fenceABI skips unless ABHED_REQUIRE_FENCE is set, and then fails rather
// than skips when the kernel cannot run the fence.
func fenceABI(t *testing.T) ABI {
	t.Helper()
	if os.Getenv(requireEnv) == "" {
		t.Skipf("set %s=1 to apply Landlock on this kernel", requireEnv)
	}
	if os.Geteuid() == 0 {
		t.Fatal("the fence refuses root; run these tests as an unprivileged user")
	}
	abi, err := Detect()
	if err != nil {
		t.Fatal(err)
	}
	if err := abi.Check(); err != nil {
		t.Fatal(err)
	}
	t.Log(abi.Describe())
	return abi
}

type layout struct {
	work, tmp, state, record, secrets, outside string
}

func newLayout(t *testing.T) layout {
	t.Helper()
	root := t.TempDir()
	l := layout{
		work:    filepath.Join(root, "work"),
		tmp:     filepath.Join(root, "cmd-tmp"),
		state:   filepath.Join(root, "home", ".abhed"),
		secrets: filepath.Join(root, "home", "secrets.env"),
		outside: filepath.Join(root, "outside"),
	}
	l.record = filepath.Join(l.state, "record.jsonl")
	for _, d := range []string{l.work, l.tmp, l.state, l.outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{l.record, l.secrets, filepath.Join(l.outside, "file")} {
		if err := os.WriteFile(f, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func (l layout) spec(t *testing.T, denyTCP bool) Spec {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Spec{
		Exec:    []string{"/usr", "/bin", "/lib", "/lib64", "/etc", self},
		Read:    []string{"/proc", "/sys"},
		Write:   []string{l.work, l.tmp},
		Devices: []string{"/dev/null"},
		Deny:    []string{l.state, l.secrets},
		DenyTCP: denyTCP,
	}
}

// run starts the launcher with spec and returns the probe's exit code.
func run(t *testing.T, s Spec, op, arg string) int {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], op, arg) // #nosec G204 -- the test binary itself
	cmd.Env = append(os.Environ(), roleEnv+"=launch", specEnv+"="+string(raw))
	out, err := cmd.CombinedOutput()
	code := cmd.ProcessState.ExitCode()
	if code > 1 {
		t.Fatalf("%s %s: the launcher or probe failed (exit %d, %v): %s", op, arg, code, err, strings.TrimSpace(string(out)))
	}
	return code
}

func TestConstantsMatchKernelHeaders(t *testing.T) {
	pairs := []struct {
		ours, theirs uint64
	}{
		{fsExecute, unix.LANDLOCK_ACCESS_FS_EXECUTE}, {fsWriteFile, unix.LANDLOCK_ACCESS_FS_WRITE_FILE},
		{fsReadFile, unix.LANDLOCK_ACCESS_FS_READ_FILE}, {fsReadDir, unix.LANDLOCK_ACCESS_FS_READ_DIR},
		{fsRemoveDir, unix.LANDLOCK_ACCESS_FS_REMOVE_DIR}, {fsRemoveFile, unix.LANDLOCK_ACCESS_FS_REMOVE_FILE},
		{fsMakeChar, unix.LANDLOCK_ACCESS_FS_MAKE_CHAR}, {fsMakeDir, unix.LANDLOCK_ACCESS_FS_MAKE_DIR},
		{fsMakeReg, unix.LANDLOCK_ACCESS_FS_MAKE_REG}, {fsMakeSock, unix.LANDLOCK_ACCESS_FS_MAKE_SOCK},
		{fsMakeFifo, unix.LANDLOCK_ACCESS_FS_MAKE_FIFO}, {fsMakeBlock, unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK},
		{fsMakeSym, unix.LANDLOCK_ACCESS_FS_MAKE_SYM}, {fsRefer, unix.LANDLOCK_ACCESS_FS_REFER},
		{fsTruncate, unix.LANDLOCK_ACCESS_FS_TRUNCATE}, {fsIoctlDev, unix.LANDLOCK_ACCESS_FS_IOCTL_DEV},
		{netBindTCP, unix.LANDLOCK_ACCESS_NET_BIND_TCP}, {netConnectTCP, unix.LANDLOCK_ACCESS_NET_CONNECT_TCP},
		{scopeAbstractUnix, unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET}, {scopeSignal, unix.LANDLOCK_SCOPE_SIGNAL},
	}
	for i, p := range pairs {
		if p.ours != p.theirs {
			t.Errorf("constant %d: ours %#x, kernel header %#x", i, p.ours, p.theirs)
		}
	}
}

func TestRestrictFilesystem(t *testing.T) {
	fenceABI(t)
	l := newLayout(t)
	fenced := l.spec(t, false)

	// Unconfined, every probe below succeeds, so a refusal under the fence
	// is the fence's doing.
	for _, c := range [][2]string{{"read", l.record}, {"read", l.secrets}, {"write", filepath.Join(l.outside, "new")}} {
		if got := run(t, Spec{}, c[0], c[1]); got != 0 {
			t.Fatalf("unconfined %s %s: exit %d", c[0], c[1], got)
		}
	}

	cases := []struct {
		name, op, path string
		allowed        bool
	}{
		{"write in the workspace", "write", filepath.Join(l.work, "out.txt"), true},
		{"write in the private temp folder", "write", filepath.Join(l.tmp, "scratch"), true},
		{"read from the system", "read", "/etc/passwd", true},
		{"read Abhed's record", "read", l.record, false},
		{"overwrite Abhed's record", "write", l.record, false},
		{"create in Abhed's state", "write", filepath.Join(l.state, "planted"), false},
		{"read the secrets file", "read", l.secrets, false},
		{"overwrite the secrets file", "write", l.secrets, false},
		{"write outside the grants", "write", filepath.Join(l.outside, "new"), false},
		{"read outside the grants", "read", filepath.Join(l.outside, "file"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := run(t, fenced, c.op, c.path)
			if want := map[bool]int{true: 0, false: 1}[c.allowed]; got != want {
				t.Fatalf("%s %s under the fence: exit %d, want %d", c.op, c.path, got, want)
			}
		})
	}
	if b, err := os.ReadFile(filepath.Join(l.work, "out.txt")); err != nil || string(b) != "x" {
		t.Fatalf("the workspace write did not land: %q, %v", b, err)
	}
	if b, _ := os.ReadFile(l.record); string(b) != "secret" {
		t.Fatalf("the record changed under the fence: %q", b)
	}
}

func TestRestrictRefusesTCPWhenNetworkOff(t *testing.T) {
	abi := fenceABI(t)
	if !abi.Network() {
		t.Skipf("Landlock ABI %d has no TCP rules; ABI 4 (Linux 6.7) is needed", abi)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	addr := ln.Addr().String()
	l := newLayout(t)

	if got := run(t, l.spec(t, false), "connect", addr); got != 0 {
		t.Fatalf("connect with the network on: exit %d, want 0", got)
	}
	if got := run(t, l.spec(t, true), "connect", addr); got != 1 {
		t.Fatalf("connect with the network off: exit %d, want 1 (refused)", got)
	}
	// A named port: newer kernels let port 0 bind under a TCP bind rule.
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().String()
	_ = free.Close()
	got := run(t, l.spec(t, true), "bind", port)
	switch {
	case got == 1:
	case abi >= 7 && got == 0:
		// Linux 6.15 (ABI 7) let this bind through on CI; seccomp refuses socket() in the fence first.
		t.Logf("bind with the network off was allowed on Landlock ABI %d; connect stays refused", abi)
	default:
		t.Fatalf("bind with the network off: exit %d, want 1 (refused)", got)
	}
}

func TestRestrictRevalidates(t *testing.T) {
	abi := fenceABI(t)
	l := newLayout(t)
	s := l.spec(t, false)
	s.Deny = append(s.Deny, filepath.Join(l.work, ".abhed"))
	if _, err := New(s, abi); !errors.Is(err, ErrSpec) {
		t.Fatalf("a workspace .abhed under the write grant was accepted: %v", err)
	}
	// A write grant that does not exist at New, then appears as a link into
	// denied state, is caught when Restrict validates again.
	extra := filepath.Join(filepath.Dir(l.work), "extra")
	s = l.spec(t, false)
	s.Write = append(s.Write, extra)
	r, err := New(s, abi)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(l.state, extra); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.Restrict() }() // its thread stays locked and is discarded
	if err := <-done; !errors.Is(err, ErrSpec) {
		t.Fatalf("Restrict applied a write grant that now links into denied state: %v", err)
	}
}
