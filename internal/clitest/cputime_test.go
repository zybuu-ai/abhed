package clitest

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestParsePSTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0:00.06": 60 * time.Millisecond, "1:02.50": time.Minute + 2500*time.Millisecond,
		"01:02:03": time.Hour + 2*time.Minute + 3*time.Second, "2-00:00:01": 48*time.Hour + time.Second,
	} {
		if got, err := parsePSTime(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "12", "a:b", "1:2:3:4"} {
		if _, err := parsePSTime(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// A process the harness started has its CPU time read, and it grows with
// work. A child, as the harness measures: an emulator such as qemu-user
// writes its own /proc/self/stat, with no CPU time, but not its children's.
func TestProcessCPU(t *testing.T) {
	if os.Getenv("ABHED_CPU_BUSY") != "" {
		x := 0
		for end := time.Now().Add(400 * time.Millisecond); time.Now().Before(end); {
			x++
		}
		time.Sleep(5 * time.Second)
		if x < 0 {
			os.Exit(1)
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessCPU$") // #nosec G204 -- the test binary
	cmd.Env = append(os.Environ(), "ABHED_CPU_BUSY=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	before, err := processCPU(cmd.Process.Pid)
	if err != nil {
		t.Skipf("no CPU time here: %v", err)
	}
	var after time.Duration
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if after, err = processCPU(cmd.Process.Pid); err != nil || after > before {
			break
		}
	}
	if err != nil || after <= before {
		t.Fatalf("before %v after %v: %v", before, after, err)
	}
}
