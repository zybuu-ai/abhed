//go:build darwin

package app

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const hideCanary = "ABHED_TEST_HIDE_CANARY=harmless-canary-value"

// TestHideEnvHelper is the process whose environment is hidden.
func TestHideEnvHelper(t *testing.T) {
	_, fd := os.LookupEnv(hiddenEnvFD)
	if os.Getenv("ABHED_TEST_HIDE_ROLE") == "" && !fd {
		return
	}
	HideEnvironment()
	os.Stdout.WriteString("ready " + os.Getenv("ABHED_TEST_HIDE_CANARY") + " " + strconv.Itoa(len(os.Getenv(hiddenEnvFD))) + "\n")
	time.Sleep(20 * time.Second)
}

// A process of the same user reading Abhed's kern.procargs2 no longer finds
// a variable Abhed was started with, while Abhed itself still reads it, under
// the same process id.
func TestHideEnvironmentKeepsItFromProcArgs(t *testing.T) {
	c := exec.Command(os.Args[0], "-test.run=^TestHideEnvHelper$") // #nosec G204 -- the test binary
	c.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "ABHED_TEST_HIDE_ROLE=hold", hideCanary}
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("the helper said nothing: %v", err)
	}
	if line != "ready harmless-canary-value 0\n" {
		t.Fatalf("the helper lost its environment or kept the descriptor variable: %q", line)
	}
	args := readProcArgs(t, c.Process.Pid)
	if bytes.Contains(args, []byte("harmless-canary-value")) || bytes.Contains(args, []byte("ABHED_TEST_HIDE_ROLE")) {
		t.Fatalf("kern.procargs2 still shows the hidden variables:\n%q", args)
	}
	if !bytes.Contains(args, []byte("PATH=/usr/bin:/bin")) || !strings.Contains(string(args), hiddenEnvFD+"=") {
		t.Fatalf("the re-executed process is not the one read:\n%q", args)
	}
}

func readProcArgs(t *testing.T, pid int) []byte {
	t.Helper()
	max, err := unix.SysctlUint32("kern.argmax")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, max)
	size := uintptr(max)
	mib := [3]int32{unix.CTL_KERN, 49, int32(pid)} // KERN_PROCARGS2
	// #nosec G103 -- sysctl(2) needs the buffer's address
	if _, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), 3,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, 0); errno != 0 {
		t.Fatalf("kern.procargs2: %v", errno)
	}
	return buf[:size]
}
