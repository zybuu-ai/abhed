package clitest

import (
	"os"
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

// This process's own CPU time reads, and grows with work.
func TestProcessCPU(t *testing.T) {
	before, err := processCPU(os.Getpid())
	if err != nil {
		t.Skipf("no CPU time here: %v", err)
	}
	x := 0
	for end := time.Now().Add(150 * time.Millisecond); time.Now().Before(end); {
		x++
	}
	after, err := processCPU(os.Getpid())
	if err != nil || after <= before || x == 0 {
		t.Fatalf("before %v after %v: %v", before, after, err)
	}
}
