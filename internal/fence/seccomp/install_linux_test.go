//go:build linux

package seccomp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The confined checks run in this test binary re-executed with stageEnv set:
// TestMain answers before any test runs, so the test process itself is never
// filtered.
const (
	stageEnv = "ABHED_SECCOMP_TEST_STAGE"
	dirEnv   = "ABHED_SECCOMP_TEST_DIR"
)

// Stages.
const (
	stageControl    = "control"      // the denied checks, with no filter
	stageDenied     = "denied"       // the denied checks, under Command(false)
	stageOrdinaryOn = "ordinary-on"  // ordinary work, under Command(true)
	stageOrdinaryNo = "ordinary-off" // ordinary work, under Command(false)
	stageSocketsOn  = "sockets-on"   // the socket checks, under Command(true)
)

func TestMain(m *testing.M) {
	if stage := os.Getenv(stageEnv); stage != "" {
		os.Exit(stageMain(stage, os.Getenv(dirEnv)))
	}
	os.Exit(m.Run())
}

// outcome is one check's result: "ok", an errno name, "absent" for a tool
// that is not installed, or what went wrong.
type outcome struct {
	Name string `json:"name"`
	Got  string `json:"got"`
}

// guarded is the policy for a stage: this process's parent, the test, stands
// in for Abhed.
func guarded(network bool) Policy {
	group, _ := unix.Getpgid(os.Getppid())
	return Command(network, os.Getppid(), group)
}

func stageMain(stage, dir string) int {
	var out []outcome
	switch stage {
	case stageControl:
		out = deniedChecks(dir)
	case stageDenied:
		if err := Install(guarded(false)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		out = append(deniedChecks(dir), outcome{"a second Install", result(Install(guarded(false)))})
	case stageSocketsOn:
		if err := Install(guarded(true)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		out = socketChecks()
	case stageOrdinaryOn, stageOrdinaryNo:
		if err := Install(guarded(stage == stageOrdinaryOn)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		out = ordinaryChecks(dir)
	default:
		fmt.Fprintf(os.Stderr, "no stage %q\n", stage)
		return 2
	}
	b, _ := json.Marshal(out)
	_, _ = os.Stdout.Write(b)
	return 0
}

func result(err error) string {
	if err == nil {
		return "ok"
	}
	var e syscall.Errno
	if errors.As(err, &e) {
		if n := unix.ErrnoName(e); n != "" {
			return n
		}
	}
	return err.Error()
}

func errnoErr(e syscall.Errno) error {
	if e == 0 {
		return nil
	}
	return e
}

// closeIfFD closes what a raw call that succeeded returned.
func closeIfFD(r, _ uintptr, e syscall.Errno) error {
	if e == 0 {
		_ = unix.Close(int(r))
	}
	return errnoErr(e)
}

func allowAllFilter() *unix.SockFprog {
	f := []unix.SockFilter{{Code: opRet, K: retAllow}}
	return &unix.SockFprog{Len: 1, Filter: &f[0]}
}

// deniedChecks try one call from each refused family, and the socket
// families; the caller compares them to what the filter should answer.
func deniedChecks(dir string) []outcome {
	var out []outcome
	add := func(name string, err error) { out = append(out, outcome{name, result(err)}) }
	pid := os.Getpid()

	mnt := filepath.Join(dir, "mnt")
	_ = os.MkdirAll(mnt, 0o700)
	err := unix.Mount("none", mnt, "tmpfs", 0, "")
	if err == nil {
		_ = unix.Unmount(mnt, 0)
	}
	add("mount", err)
	fstype, _ := unix.BytePtrFromString("tmpfs")
	add("fsopen", closeIfFD(unix.Syscall(unix.SYS_FSOPEN, uintptr(unsafe.Pointer(fstype)), 0, 0)))
	add("pivot_root", unix.PivotRoot(mnt, mnt))
	add("chroot", unix.Chroot(mnt))

	add("unshare(CLONE_NEWUSER)", unix.Unshare(unix.CLONE_NEWUSER))
	add("unshare(CLONE_NEWNS)", unix.Unshare(unix.CLONE_NEWNS))
	if fd, err := unix.Open("/proc/self/ns/uts", unix.O_RDONLY|unix.O_CLOEXEC, 0); err == nil {
		add("setns", unix.Setns(fd, 0))
		_ = unix.Close(fd)
	} else {
		add("setns", fmt.Errorf("opening the namespace: %w", err))
	}
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER}
	err = cmd.Start()
	if err == nil {
		_ = cmd.Wait()
	}
	add("clone(CLONE_NEWUSER)", err)
	_, _, e := unix.Syscall(unix.SYS_CLONE3, 0, 0, 0)
	add("clone3", errnoErr(e))

	add("ptrace attach", ptraceAttach())
	local, remote := make([]byte, 8), []byte("abcdefgh")
	_, err = unix.ProcessVMReadv(pid, []unix.Iovec{{Base: &local[0], Len: 8}}, []unix.RemoteIovec{{Base: uintptr(unsafe.Pointer(&remote[0])), Len: 8}}, 0)
	add("process_vm_readv", err)
	_, _, e = unix.Syscall6(unix.SYS_KCMP, uintptr(pid), uintptr(pid), 0, 0, 0, 0)
	add("kcmp", errnoErr(e))
	if pfd, err := unix.PidfdOpen(pid, 0); err == nil {
		fd, err := unix.PidfdGetfd(pfd, 0, 0)
		if err == nil {
			_ = unix.Close(fd)
		}
		add("pidfd_getfd", err)
		_ = unix.Close(pfd)
	} else {
		add("pidfd_getfd", fmt.Errorf("pidfd_open: %w", err))
	}

	var attr [128]byte
	add("bpf", closeIfFD(unix.Syscall(unix.SYS_BPF, 0, uintptr(unsafe.Pointer(&attr[0])), uintptr(len(attr)))))
	add("perf_event_open", closeIfFD(unix.Syscall6(unix.SYS_PERF_EVENT_OPEN, 0, 0, ^uintptr(0), ^uintptr(0), 0, 0)))
	add("userfaultfd", closeIfFD(unix.Syscall(unix.SYS_USERFAULTFD, 1 /* UFFD_USER_MODE_ONLY */ |unix.O_CLOEXEC, 0, 0)))
	var params [120]byte
	add("io_uring_setup", closeIfFD(unix.Syscall(unix.SYS_IO_URING_SETUP, 1, uintptr(unsafe.Pointer(&params[0])), 0)))
	empty, _ := unix.BytePtrFromString("")
	add("finit_module", errnoErr(callErrno(unix.Syscall(unix.SYS_FINIT_MODULE, ^uintptr(0), uintptr(unsafe.Pointer(empty)), 0))))
	add("kexec_load", errnoErr(callErrno(unix.Syscall6(unix.SYS_KEXEC_LOAD, 0, 0, 0, 0, 0, 0))))

	_, err = unix.KeyctlGetKeyringID(unix.KEY_SPEC_SESSION_KEYRING, false)
	add("keyctl", err)
	_, err = unix.AddKey("user", "abhed-seccomp-test", []byte("x"), unix.KEY_SPEC_PROCESS_KEYRING)
	add("add_key", err)

	prog := allowAllFilter()
	add("seccomp(SET_MODE_FILTER)", errnoErr(callErrno(unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(prog))))))
	add("prctl(PR_SET_SECCOMP)", unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(prog)), 0, 0))
	out = append(out, signalChecks()...)
	out = append(out, socketChecks()...)
	add("getpid", errnoErr(callErrno(unix.Syscall(unix.SYS_GETPID, 0, 0, 0))))
	return out
}

// signalChecks send signal 0, which only checks the target could be
// signalled, at the stage's parent standing in for Abhed, and at itself.
func signalChecks() []outcome {
	var out []outcome
	add := func(name string, err error) { out = append(out, outcome{name, result(err)}) }
	abhed, self := os.Getppid(), os.Getpid()
	group, _ := unix.Getpgid(abhed)
	add("kill(Abhed)", unix.Kill(abhed, 0))
	add("kill(Abhed's group)", unix.Kill(-group, 0))
	add("kill(-1)", unix.Kill(-1, 0))
	add("tkill(Abhed)", errnoErr(callErrno(unix.Syscall(unix.SYS_TKILL, uintptr(abhed), 0, 0))))
	add("tgkill(Abhed)", unix.Tgkill(abhed, abhed, 0))
	var info [128]byte
	code := int32(-1) // SI_QUEUE
	*(*int32)(unsafe.Pointer(&info[8])) = code
	add("rt_sigqueueinfo(Abhed)", errnoErr(callErrno(unix.Syscall(unix.SYS_RT_SIGQUEUEINFO, uintptr(abhed), 0, uintptr(unsafe.Pointer(&info[0]))))))
	add("rt_tgsigqueueinfo(Abhed)", errnoErr(callErrno(unix.Syscall6(unix.SYS_RT_TGSIGQUEUEINFO, uintptr(abhed), uintptr(abhed), 0, uintptr(unsafe.Pointer(&info[0])), 0, 0))))
	if pfd, err := unix.PidfdOpen(self, 0); err == nil {
		add("pidfd_send_signal", unix.PidfdSendSignal(pfd, 0, nil, 0))
		_ = unix.Close(pfd)
	} else {
		add("pidfd_send_signal", fmt.Errorf("pidfd_open: %w", err))
	}
	add("kill(self)", unix.Kill(self, 0))
	add("kill(own group)", unix.Kill(0, 0))
	return out
}

// socketChecks create one socket of each kind, and a socketpair.
func socketChecks() []outcome {
	var out []outcome
	for _, s := range []struct {
		name           string
		domain, typ, p int
	}{
		{"socket(AF_INET stream)", unix.AF_INET, unix.SOCK_STREAM, 0},
		{"socket(AF_INET datagram)", unix.AF_INET, unix.SOCK_DGRAM, 0},
		{"socket(NETLINK_ROUTE)", unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_ROUTE},
		{"socket(AF_UNIX stream)", unix.AF_UNIX, unix.SOCK_STREAM, 0},
		{"socket(AF_UNIX datagram)", unix.AF_UNIX, unix.SOCK_DGRAM, 0},
		{"socket(AF_PACKET)", unix.AF_PACKET, unix.SOCK_RAW, 0},
		{"socket(AF_VSOCK)", unix.AF_VSOCK, unix.SOCK_STREAM, 0},
		{"socket(NETLINK_AUDIT)", unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_AUDIT},
		{"socket(NETLINK_KOBJECT_UEVENT)", unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_KOBJECT_UEVENT},
	} {
		fd, err := unix.Socket(s.domain, s.typ|unix.SOCK_CLOEXEC, s.p)
		if err == nil {
			_ = unix.Close(fd)
		}
		out = append(out, outcome{s.name, result(err)})
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err == nil {
		_, _ = unix.Close(fds[0]), unix.Close(fds[1])
	}
	return append(out, outcome{"socketpair(AF_UNIX)", result(err)})
}

func callErrno(_, _ uintptr, e syscall.Errno) syscall.Errno { return e }

// ptraceAttach tries to attach to a child of this process, which an
// unconfined process of the same user may do.
func ptraceAttach() error {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		return fmt.Errorf("starting a child: %w", err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	err := unix.PtraceAttach(child.Process.Pid)
	if err == nil {
		var ws unix.WaitStatus
		_, _ = unix.Wait4(child.Process.Pid, &ws, unix.WALL, nil)
		_ = unix.PtraceDetach(child.Process.Pid)
	}
	return err
}

// ordinaryChecks do the work a build does. Tools that are not installed
// answer "absent".
func ordinaryChecks(dir string) []outcome {
	var out []outcome
	add := func(name, got string) { out = append(out, outcome{name, got}) }
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir, "LANG=C",
		"GOCACHE=" + filepath.Join(dir, "gocache"), "GOPATH=" + filepath.Join(dir, "gopath"),
		"GOTOOLCHAIN=local", "GOFLAGS=", "CGO_ENABLED=0"}
	// run executes a command line in wd and returns "ok" when its trimmed
	// output is want.
	run := func(wd, want string, name string, args ...string) string {
		if _, err := exec.LookPath(name); err != nil {
			return "absent"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Env = wd, env
		b, err := cmd.CombinedOutput()
		got := strings.TrimSpace(string(b))
		if err != nil {
			return fmt.Sprintf("%v: %s", err, got)
		}
		if want != "*" && got != want {
			return fmt.Sprintf("output %q, want %q", got, want)
		}
		return "ok"
	}
	write := func(name, body string) {
		_ = os.MkdirAll(filepath.Dir(name), 0o700)
		_ = os.WriteFile(name, []byte(body), 0o600)
	}

	add("sh pipeline and redirection", run(dir, "b\n3", "sh", "-c", "printf a | tr a b; echo; echo $((1+2)) > n; cat n; ls / >/dev/null"))
	add("child inherits the filter", run(dir, "NoNewPrivs: 1\nSeccomp: 2", "sh", "-c",
		"grep -E '^(Seccomp|NoNewPrivs):' /proc/self/status | tr -s '\\t ' ' ' | sort"))
	add("bash", run(dir, "ok", "bash", "-c", "set -euo pipefail; a=(x y); [[ ${#a[@]} == 2 ]] && echo ok | cat"))
	add("go version", run(dir, "*", "go", "version"))

	hello := filepath.Join(dir, "hello")
	write(filepath.Join(hello, "go.mod"), "module hello\n\ngo 1.21\n")
	write(filepath.Join(hello, "main.go"), "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello\") }\n")
	if got := run(hello, "", "go", "build", "-o", "hello", "."); got != "ok" {
		add("go build and run", got)
	} else {
		add("go build and run", run(hello, "hello", filepath.Join(hello, "hello")))
	}

	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(repo, 0o700)
	git := func(want string, args ...string) string {
		return run(repo, want, "git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "init.defaultBranch=main"}, args...)...)
	}
	gitSteps := []string{git("*", "init", "-q")}
	write(filepath.Join(repo, "a.txt"), "one\n")
	gitSteps = append(gitSteps, git("", "add", "a.txt"), git("*", "commit", "-q", "-m", "first"))
	write(filepath.Join(repo, "a.txt"), "two\n")
	gitSteps = append(gitSteps, git("M a.txt", "status", "--porcelain"), git("1", "rev-list", "--count", "HEAD"))
	add("git init, commit and status", firstFailure(gitSteps))

	write(filepath.Join(dir, "mk", "Makefile"), "all: out\n\t@cat out\nout:\n\t@echo made > out\n")
	add("make", run(filepath.Join(dir, "mk"), "made", "make", "-s"))
	write(filepath.Join(dir, "c", "hello.c"), "#include <stdio.h>\n#include <pthread.h>\nstatic void *f(void *a){return a;}\nint main(void){pthread_t t;pthread_create(&t,0,f,0);pthread_join(t,0);puts(\"c ok\");return 0;}\n")
	if got := run(filepath.Join(dir, "c"), "", "cc", "-pthread", "-o", "hello", "hello.c"); got != "ok" {
		add("cc compile and run", got)
	} else {
		add("cc compile and run", run(filepath.Join(dir, "c"), "c ok", filepath.Join(dir, "c", "hello")))
	}
	add("python3", run(dir, "py 2", "python3", "-c",
		"import subprocess,threading,tempfile,os\nr=[]\nt=threading.Thread(target=lambda:r.append(1));t.start();t.join()\n"+
			"f=tempfile.NamedTemporaryFile(dir='.');f.write(b'x');f.flush()\n"+
			"print(subprocess.check_output(['echo','py']).decode().strip(), len(r)+1)"))
	add("node", run(dir, "node 2", "node", "-e",
		"const cp=require('child_process'),fs=require('fs');fs.writeFileSync('n.txt','2');"+
			"fs.readFile('n.txt','utf8',(e,d)=>{if(e)throw e;console.log(cp.execSync('echo node').toString().trim()+' '+d)})"))
	// Network use, which only the network-on stage expects to work.
	add("python3 socket module", run(dir, "ok", "python3", "-c",
		"import socket\ns=socket.socket();s.bind(('127.0.0.1',0));s.listen();c=socket.create_connection(s.getsockname());c.close();s.close();print('ok')"))
	add("node tcp", run(dir, "ok", "node", "-e",
		"const net=require('net');const s=net.createServer(c=>c.end()).listen(0,'127.0.0.1',()=>"+
			"net.connect(s.address().port,'127.0.0.1').on('close',()=>{s.close();console.log('ok')}).on('error',e=>{throw e}).resume())"))

	add("file io", fileIO(dir))
	add("threads", threads())
	add("socketpair", socketpair())
	add("tcp loopback", tcpLoopback())
	add("unix socket listen", unixSocket(dir))
	_, err := net.Interfaces() // route netlink
	add("route netlink (interfaces)", result(err))
	return out
}

// socketpair passes bytes over a socketpair, as pipes between processes do.
func socketpair() string {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return result(err)
	}
	defer func() { _, _ = unix.Close(fds[0]), unix.Close(fds[1]) }()
	if _, err := unix.Write(fds[0], []byte("ping")); err != nil {
		return result(err)
	}
	buf := make([]byte, 4)
	if n, err := unix.Read(fds[1], buf); err != nil || string(buf[:n]) != "ping" {
		return fmt.Sprintf("read %q: %v", buf[:n], err)
	}
	return "ok"
}

func firstFailure(steps []string) string {
	for _, s := range steps {
		if s != "ok" {
			return s
		}
	}
	return "ok"
}

func fileIO(dir string) string {
	d := filepath.Join(dir, "io")
	if err := os.MkdirAll(filepath.Join(d, "sub"), 0o700); err != nil {
		return result(err)
	}
	a, b := filepath.Join(d, "a"), filepath.Join(d, "sub", "b")
	if err := os.WriteFile(a, bytes.Repeat([]byte("x"), 1<<20), 0o600); err != nil {
		return result(err)
	}
	if err := os.Rename(a, b); err != nil {
		return result(err)
	}
	if err := os.Symlink(b, a); err != nil {
		return result(err)
	}
	got, err := os.ReadFile(a)
	if err != nil || len(got) != 1<<20 {
		return fmt.Sprintf("read back %d bytes: %v", len(got), err)
	}
	if err := os.Truncate(b, 0); err != nil {
		return result(err)
	}
	return result(os.RemoveAll(d))
}

// threads makes the runtime start new OS threads under the filter.
func threads() string {
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	tids := map[int]bool{}
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			mu.Lock()
			tids[unix.Gettid()] = true
			mu.Unlock()
			<-start
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(start)
	wg.Wait()
	if len(tids) != n {
		return fmt.Sprintf("%d goroutines locked to only %d threads", n, len(tids))
	}
	return "ok"
}

func tcpLoopback() string {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return result(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_, _ = io.Copy(c, c)
			_ = c.Close()
		}
	}()
	return echo("tcp4", ln.Addr().String())
}

func unixSocket(dir string) string {
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		return result(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_, _ = io.Copy(c, c)
			_ = c.Close()
		}
	}()
	return echo("unix", ln.Addr().String())
}

func echo(network, addr string) string {
	c, err := net.DialTimeout(network, addr, 5*time.Second)
	if err != nil {
		return result(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		return result(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		return fmt.Sprintf("echo %q: %v", buf, err)
	}
	return "ok"
}

// requireFence skips unless ABHED_REQUIRE_FENCE is set, as on a host meant to
// qualify for the fence.
func requireFence(t *testing.T) {
	t.Helper()
	if os.Getenv("ABHED_REQUIRE_FENCE") == "" {
		t.Skip("set ABHED_REQUIRE_FENCE=1 on a host that should qualify for the fence")
	}
	if _, ok := arches[runtime.GOARCH]; !ok {
		t.Fatalf("the fence needs amd64 or arm64, not %s", runtime.GOARCH)
	}
}

func runStage(t *testing.T, stage string) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/proc/self/exe")
	cmd.Env = append(os.Environ(), stageEnv+"="+stage, dirEnv+"="+t.TempDir())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("stage %s: %v: %s", stage, err, stderr.String())
	}
	var out []outcome
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stage %s: %v: %q", stage, err, stdout.String())
	}
	m := map[string]string{}
	for _, o := range out {
		m[o.Name] = o.Got
	}
	return m
}

// Under Command(false) each refused family answers its errno, while the same
// calls with no filter answer otherwise wherever this host lets them through.
func TestFilterRefusesEachFamily(t *testing.T) {
	requireFence(t)
	want := map[string]string{
		"mount": "EPERM", "fsopen": "EPERM", "pivot_root": "EPERM", "chroot": "EPERM",
		"unshare(CLONE_NEWUSER)": "EPERM", "unshare(CLONE_NEWNS)": "EPERM", "setns": "EPERM",
		"clone(CLONE_NEWUSER)": "EPERM", "clone3": "ENOSYS",
		"ptrace attach": "EPERM", "process_vm_readv": "EPERM", "kcmp": "EPERM", "pidfd_getfd": "EPERM",
		"bpf": "EPERM", "perf_event_open": "EPERM", "userfaultfd": "EPERM", "io_uring_setup": "EPERM",
		"finit_module": "EPERM", "kexec_load": "EPERM", "keyctl": "EPERM", "add_key": "EPERM",
		"seccomp(SET_MODE_FILTER)": "EPERM", "prctl(PR_SET_SECCOMP)": "EPERM",
		"socket(AF_INET stream)": "EPERM", "socket(AF_INET datagram)": "EPERM", "socket(NETLINK_ROUTE)": "EPERM",
		"socket(AF_UNIX stream)": "EPERM", "socket(AF_UNIX datagram)": "EPERM",
		"socket(AF_PACKET)": "EPERM", "socket(AF_VSOCK)": "EPERM",
		"socket(NETLINK_AUDIT)": "EPERM", "socket(NETLINK_KOBJECT_UEVENT)": "EPERM",
		"socketpair(AF_UNIX)": "ok", "getpid": "ok",
		"kill(Abhed)": "EPERM", "kill(Abhed's group)": "EPERM", "kill(-1)": "EPERM", "tkill(Abhed)": "EPERM",
		"tgkill(Abhed)": "EPERM", "rt_sigqueueinfo(Abhed)": "EPERM", "rt_tgsigqueueinfo(Abhed)": "EPERM",
		"pidfd_send_signal": "EPERM", "kill(self)": "ok", "kill(own group)": "ok",
	}
	control := runStage(t, stageControl)
	denied := runStage(t, stageDenied)
	var proved, refusals []string
	for name, w := range want {
		if w != "ok" {
			refusals = append(refusals, name)
		}
		got := denied[name]
		t.Logf("%-32s filtered %-14s unfiltered %s", name, got, control[name])
		if got != w {
			t.Errorf("%s under the filter: got %q, want %q", name, got, w)
		}
		if w != "ok" && control[name] != w {
			proved = append(proved, name)
		}
	}
	if !strings.Contains(denied["a second Install"], "EPERM") {
		t.Errorf("a second Install: %q", denied["a second Install"])
	}
	// The checks must be able to tell: a host that refused everything anyway
	// would pass the table without the filter doing anything.
	for _, name := range []string{"clone3", "kcmp", "process_vm_readv", "keyctl", "seccomp(SET_MODE_FILTER)",
		"socket(AF_INET stream)", "socket(AF_UNIX stream)", "kill(Abhed)", "tgkill(Abhed)", "pidfd_send_signal"} {
		if control[name] == want[name] {
			t.Errorf("%s answers %s with no filter too, so the check proves nothing here", name, want[name])
		}
	}
	t.Logf("refusals the filter made that the host would not have: %d of %d", len(proved), len(refusals))
}

// With the network on, inet, inet6 and route netlink sockets work and every
// other family, unix included, is refused; socketpair works.
func TestNetworkOnSockets(t *testing.T) {
	requireFence(t)
	want := map[string]string{
		"socket(AF_INET stream)": "ok", "socket(AF_INET datagram)": "ok", "socket(NETLINK_ROUTE)": "ok",
		"socket(AF_UNIX stream)": "EPERM", "socket(AF_UNIX datagram)": "EPERM",
		"socket(AF_PACKET)": "EPERM", "socket(AF_VSOCK)": "EPERM",
		"socket(NETLINK_AUDIT)": "EPERM", "socket(NETLINK_KOBJECT_UEVENT)": "EPERM",
		"socketpair(AF_UNIX)": "ok",
	}
	got := runStage(t, stageSocketsOn)
	for name, w := range want {
		t.Logf("%-32s %s", name, got[name])
		if got[name] != w {
			t.Errorf("%s with the network on: got %q, want %q", name, got[name], w)
		}
	}
}

// refused reports whether a check failed with the filter's EPERM, however
// the tool worded it.
func refused(got string) bool {
	return strings.Contains(got, "EPERM") || strings.Contains(strings.ToLower(got), "operation not permitted")
}

// Ordinary build work still succeeds under the filter, with the network on
// and off; network use works only with it on, and unix sockets never.
func TestOrdinaryWorkUnderFilter(t *testing.T) {
	requireFence(t)
	required := []string{"sh pipeline and redirection", "child inherits the filter", "go version", "go build and run",
		"git init, commit and status", "file io", "threads", "socketpair"}
	optional := []string{"bash", "make", "cc compile and run", "python3", "node"}
	network := []string{"tcp loopback", "route netlink (interfaces)", "python3 socket module", "node tcp"}
	for _, stage := range []string{stageOrdinaryOn, stageOrdinaryNo} {
		got := runStage(t, stage)
		for _, name := range required {
			t.Logf("%s %-30s %s", stage, name, got[name])
			if got[name] != "ok" {
				t.Errorf("%s: %s: %s", stage, name, got[name])
			}
		}
		for _, name := range optional {
			t.Logf("%s %-30s %s", stage, name, got[name])
			if got[name] != "ok" && got[name] != "absent" {
				t.Errorf("%s: %s: %s", stage, name, got[name])
			}
		}
		for _, name := range network {
			t.Logf("%s %-30s %s", stage, name, got[name])
			switch {
			case got[name] == "absent":
			case stage == stageOrdinaryOn && got[name] != "ok":
				t.Errorf("%s: %s: %s", stage, name, got[name])
			case stage == stageOrdinaryNo && !refused(got[name]):
				t.Errorf("%s: %s was not refused: %s", stage, name, got[name])
			}
		}
		name := "unix socket listen"
		t.Logf("%s %-30s %s", stage, name, got[name])
		if !refused(got[name]) {
			t.Errorf("%s: %s was not refused: %s", stage, name, got[name])
		}
	}
}
