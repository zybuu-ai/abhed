package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const procArgsCanary = "ABHED_TEST_PROCARGS_CANARY=harmless-canary-value"

// TestProcArgsHelper is the holder and the probe for TestSeatbeltProcArgsExposure.
func TestProcArgsHelper(t *testing.T) {
	switch os.Getenv("ABHED_TEST_PROCARGS_ROLE") {
	case "hold":
		time.Sleep(30 * time.Second)
	case "probe":
		pid, _ := strconv.Atoi(os.Getenv("ABHED_TEST_PROCARGS_PID"))
		args, ok := procArgs(int32(pid))
		switch {
		case !ok:
			fmt.Println("PROCARGS_REFUSED")
		case bytes.Contains(args, []byte(procArgsCanary)):
			fmt.Println("PROCARGS_ENV_SEEN")
		default:
			fmt.Println("PROCARGS_ENV_HIDDEN")
		}
	}
}

// Seatbelt has no rule that refuses kern.procargs2, not even a blanket
// (deny sysctl-read), so a command reads the environment of any same-user
// process the system does not restrict; the guide says so. If this fails,
// macOS started refusing it and the guide can say more.
func TestSeatbeltProcArgsExposure(t *testing.T) {
	ws := workspace(t)
	s := NewProcess(DefaultPolicy(ws))
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	hold := func(argv ...string) int {
		c := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- the test binary or /bin/sleep
		c.Env = []string{"ABHED_TEST_PROCARGS_ROLE=hold", procArgsCanary}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
		return c.Process.Pid
	}
	ordinary := hold(os.Args[0], "-test.run=^TestProcArgsHelper$")
	apple := hold("/bin/sleep", "30")
	time.Sleep(300 * time.Millisecond)

	probe := func(pid int) string {
		return fmt.Sprintf("ABHED_TEST_PROCARGS_ROLE=probe ABHED_TEST_PROCARGS_PID=%d %q -test.run='^TestProcArgsHelper$'", pid, os.Args[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name string
		run  func(script string) ([]byte, error)
	}{
		{"command", func(script string) ([]byte, error) { return s.Command(ctx, ws, script).CombinedOutput() }},
		{"stdio server", func(script string) ([]byte, error) {
			// #nosec G204 -- the server profile around the probe
			return exec.CommandContext(ctx, "sandbox-exec", "-p", serverProfile(1), "/bin/sh", "-c", script).CombinedOutput()
		}},
	} {
		out, err := tc.run(probe(ordinary))
		if !strings.Contains(string(out), "PROCARGS_ENV_SEEN") {
			t.Errorf("%s: another process's environment was not readable (err %v); tighten the guide:\n%s", tc.name, err, out)
		}
		out, err = tc.run(probe(apple))
		if !strings.Contains(string(out), "PROCARGS_ENV_HIDDEN") {
			t.Errorf("%s: a program Apple ships showed its environment (err %v):\n%s", tc.name, err, out)
		}
	}
}
