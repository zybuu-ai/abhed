//go:build linux

package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/zybuu-ai/abhed/internal/fence/mountns"
)

// The helper answers before anything else in any binary linking this package,
// and exits: it is only ever a child confined for one check.
func init() {
	if len(os.Args) > 1 && os.Args[1] == HelperArg {
		os.Exit(helperMain(os.Args[2:]))
	}
}

// Helper stages, the second argument after HelperArg.
const (
	stageNNP      = "nnp"      // set no_new_privs, then exec stageChild
	stageChild    = "child"    // report this process's ids, capabilities and flags
	stageLandlock = "landlock" // <allowed> <denied>: the filesystem round trip
	stageTCP      = "tcp"      // the TCP refusal round trip
	stageSeccomp  = "seccomp"  // apply a trivial filter and see it act
	stageMounts   = "mounts"   // <dir>: hold paths read-only in a namespace of its own
)

// helperOut is one stage's answer, a single JSON line on stdout.
type helperOut struct {
	OK     bool              `json:"ok"`
	Reason string            `json:"reason"`
	Value  string            `json:"value,omitempty"`
	Status map[string]string `json:"status,omitempty"` // stageChild: /proc/self/status
}

func helperMain(args []string) int {
	// Landlock, seccomp and no_new_privs bind the calling thread.
	runtime.LockOSThread()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "abhed: the fence probe helper was started without a stage")
		return 2
	}
	var out helperOut
	switch args[0] {
	case stageNNP:
		out = helperNNP()
	case stageChild:
		out = helperChild()
	case stageLandlock:
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "abhed: the fence probe's landlock stage needs two folders")
			return 2
		}
		out = helperLandlock(args[1], args[2])
	case stageTCP:
		out = helperTCP()
	case stageSeccomp:
		out = helperSeccomp()
	case stageMounts:
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "abhed: the fence probe's mounts stage needs a folder")
			return 2
		}
		if v, err := mountns.SelfTest(args[1]); err != nil {
			out = failed("%v", err)
		} else {
			out = helperOut{OK: true, Reason: v}
		}
	default:
		fmt.Fprintf(os.Stderr, "abhed: the fence probe helper has no stage %q\n", args[0])
		return 2
	}
	b, _ := json.Marshal(out)
	_, _ = os.Stdout.Write(append(b, '\n'))
	return 0
}

func failed(format string, a ...any) helperOut {
	return helperOut{Reason: fmt.Sprintf(format, a...)}
}

// helperNNP sets no_new_privs and becomes the would-be command, as the
// launcher does, so the child stage sees what a command would hold.
func helperNNP() helperOut {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return failed("setting no_new_privs: %v", err)
	}
	if v, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0); err != nil || v != 1 {
		return failed("no_new_privs did not stick (read back %d, %v)", v, err)
	}
	argv := []string{"/proc/self/exe", HelperArg, stageChild}
	err := unix.Exec("/proc/self/exe", argv, []string{}) // #nosec G204 -- this binary, re-executed as the probe's own child stage
	return failed("starting the child stage: %v", err)
}

// helperChild reports the fields of /proc/self/status the probe judges.
func helperChild() helperOut {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return failed("reading /proc/self/status: %v", err)
	}
	st := map[string]string{}
	for line := range strings.SplitSeq(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch k {
		case "Uid", "Gid", "CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb", "NoNewPrivs", "Seccomp":
			st[k] = strings.Join(strings.Fields(v), " ")
		}
	}
	return helperOut{OK: true, Status: st}
}

// Filesystem rights by the Landlock ABI that introduced them.
const fsRightsV1 = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR |
	unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
	unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
	unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_SYM

// handledFS is every filesystem right the ABI knows, each refused where no
// rule grants it.
func handledFS(abi int) uint64 {
	r := uint64(fsRightsV1)
	if abi >= 2 {
		r |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		r |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		r |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return r
}

// landlockABI is the Landlock ABI this kernel implements, or why it has none.
func landlockABI() (int, error) {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		switch {
		case errors.Is(errno, unix.ENOSYS):
			return 0, errors.New("this kernel has no Landlock")
		case errors.Is(errno, unix.EOPNOTSUPP):
			return 0, errors.New("this kernel has Landlock built in but turned off (add landlock to the lsm= boot parameter)")
		}
		return 0, fmt.Errorf("the kernel refused Landlock: %w", errno)
	}
	return int(abi), nil
}

// restrictSelf puts this thread, with no_new_privs, in a Landlock domain that
// handles fs and net and grants fs rights beneath the allow folders only.
func restrictSelf(fs, netRights uint64, allow ...string) error {
	attr := unix.LandlockRulesetAttr{Access_fs: fs, Access_net: netRights}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0) // #nosec G103 -- the kernel ABI's struct, passed to the syscall it is for
	if errno != 0 {
		return fmt.Errorf("creating the ruleset: %w", errno)
	}
	ruleset := int(fd)
	defer func() { _ = unix.Close(ruleset) }()
	for _, p := range allow {
		dir, err := unix.Open(p, unix.O_PATH|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
		if err != nil {
			return fmt.Errorf("opening %s: %w", p, err)
		}
		rule := unix.LandlockPathBeneathAttr{Allowed_access: fs, Parent_fd: int32(dir)} // #nosec G115 -- a file descriptor fits
		_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH,
			uintptr(unsafe.Pointer(&rule)), 0, 0, 0) // #nosec G103 -- the kernel ABI's struct, passed to the syscall it is for
		_ = unix.Close(dir)
		if errno != 0 {
			return fmt.Errorf("granting %s: %w", p, errno)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("setting no_new_privs: %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); errno != 0 {
		return fmt.Errorf("entering the domain: %w", errno)
	}
	return nil
}

// Names the landlock stage uses in its two folders.
const (
	fileAllowed  = "allowed"  // written inside the grant
	fileDenied   = "denied"   // a write outside it, which must not land
	fileExisting = "existing" // made by Run outside the grant, for truncation
)

// helperLandlock confines itself to allowed, then writes inside it, which
// must succeed, and outside it, which must be refused, as must a truncation
// outside it where the ABI controls truncation.
func helperLandlock(allowed, denied string) helperOut {
	abi, err := landlockABI()
	if err != nil {
		return failed("%v", err)
	}
	if err := restrictSelf(handledFS(abi), 0, allowed); err != nil {
		return failed("%v", err)
	}
	if err := os.WriteFile(filepath.Join(allowed, fileAllowed), []byte("ok"), 0o600); err != nil { // #nosec G703 -- a temp folder Run made, inside the Landlock domain under test
		return failed("a write inside the granted folder failed: %v", err)
	}
	err = os.WriteFile(filepath.Join(denied, fileDenied), []byte("escaped"), 0o600) // #nosec G703 -- a temp folder Run made; this write must be refused
	if err == nil {
		return failed("a write outside the granted folder succeeded")
	}
	if !errors.Is(err, unix.EACCES) {
		return failed("a write outside the granted folder failed, but not with a Landlock refusal: %v", err)
	}
	value := "write inside allowed; write outside refused (EACCES)"
	if abi >= 3 {
		err := unix.Truncate(filepath.Join(denied, fileExisting), 0)
		if err == nil {
			return failed("a truncation outside the granted folder succeeded")
		}
		if !errors.Is(err, unix.EACCES) {
			return failed("a truncation outside the granted folder failed, but not with a Landlock refusal: %v", err)
		}
		value += "; truncate outside refused (EACCES)"
	}
	return helperOut{OK: true, Reason: "Landlock allowed the granted write and refused the others", Value: value}
}

// helperTCP checks loopback TCP works, then enters a domain refusing TCP and
// requires both a connect and a bind to be refused.
func helperTCP() helperOut {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return failed("no loopback TCP to test with: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	if err := tcpConnect(port); err != nil {
		return failed("a loopback connect failed before confinement, so a refusal would prove nothing: %v", err)
	}
	if err := restrictSelf(0, unix.LANDLOCK_ACCESS_NET_BIND_TCP|unix.LANDLOCK_ACCESS_NET_CONNECT_TCP); err != nil {
		return failed("%v", err)
	}
	if err := tcpConnect(port); !errors.Is(err, unix.EACCES) {
		return failed("a TCP connect under the domain was not refused (got %v)", err)
	}
	if err := tcpBind(); !errors.Is(err, unix.EACCES) {
		return failed("a TCP bind under the domain was not refused (got %v)", err)
	}
	return helperOut{OK: true, Reason: "Landlock refused TCP connect and bind", Value: "connect and bind refused (EACCES)"}
}

func tcpSocket() (int, error) {
	return unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
}

func tcpConnect(port int) error {
	s, err := tcpSocket()
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(s) }()
	return unix.Connect(s, &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}})
}

func tcpBind() error {
	s, err := tcpSocket()
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(s) }()
	return unix.Bind(s, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}})
}

// seccompArch is the audit architecture of this build, which a filter checks
// before trusting a syscall number.
func seccompArch() (uint32, bool) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, true
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, true
	}
	return 0, false
}

// probeErrno is what the trivial filter makes getppid return: an errno that
// call never gives on its own.
const probeErrno = unix.EXDEV

// helperSeccomp applies a filter refusing getppid with probeErrno and
// requires the next getppid to be refused.
func helperSeccomp() helperOut {
	before, err := unix.PrctlRetInt(unix.PR_GET_SECCOMP, 0, 0, 0, 0)
	if err != nil {
		return failed("reading the seccomp mode: %v", err)
	}
	arch, ok := seccompArch()
	if !ok {
		return failed("no seccomp architecture for %s", runtime.GOARCH)
	}
	// seccomp_data: nr at offset 0, arch at offset 4.
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 3, K: arch},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: unix.SYS_GETPPID},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(probeErrno)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]} // #nosec G115 -- six instructions fit
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return failed("setting no_new_privs: %v", err)
	}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&prog)), 0, 0); err != nil { // #nosec G103 -- the kernel ABI's struct, passed to the call it is for
		return failed("applying a seccomp filter: %v", err)
	}
	if _, _, errno := unix.RawSyscall(unix.SYS_GETPPID, 0, 0, 0); errno != probeErrno {
		return failed("the seccomp filter did not act: getppid gave errno %d, not %d", errno, probeErrno)
	}
	after, _ := unix.PrctlRetInt(unix.PR_GET_SECCOMP, 0, 0, 0, 0)
	return helperOut{OK: true, Reason: "a seccomp filter applied and refused the call it names",
		Value: fmt.Sprintf("mode %d before, %d after; getppid refused (EXDEV)", before, after)}
}
