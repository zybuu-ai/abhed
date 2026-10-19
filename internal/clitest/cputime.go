package clitest

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// CPUTime is the user and system time the binary has used so far, read
// while it runs (a 10 ms resolution on both systems). Unlike the wall clock
// it does not grow when the machine is busy with other work.
func (r *Run) CPUTime() (time.Duration, error) {
	if r.cmd == nil || r.cmd.Process == nil {
		return 0, fmt.Errorf("clitest: the binary is not running")
	}
	return processCPU(r.cmd.Process.Pid)
}

// processCPU reads pid's CPU time: /proc on Linux, ps elsewhere.
func processCPU(pid int) (time.Duration, error) {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)) // #nosec G304 -- a process this harness started
		if err != nil {
			return 0, err
		}
		// The command name may hold spaces; the fields after it are fixed.
		s := string(data)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(f) < 13 {
			return 0, fmt.Errorf("clitest: /proc/%d/stat is short", pid)
		}
		ut, err1 := strconv.ParseInt(f[11], 10, 64)
		st, err2 := strconv.ParseInt(f[12], 10, 64)
		if err1 != nil || err2 != nil {
			return 0, fmt.Errorf("clitest: /proc/%d/stat: %q %q", pid, f[11], f[12])
		}
		return time.Duration(ut+st) * 10 * time.Millisecond, nil // USER_HZ is 100 on Linux
	}
	out, err := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output() // #nosec G204 -- ps on a process this harness started
	if err != nil {
		return 0, err
	}
	return parsePSTime(strings.TrimSpace(string(out)))
}

// parsePSTime reads ps's time column: [[dd-]hh:]mm:ss[.hh].
func parsePSTime(s string) (time.Duration, error) {
	var days int64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.ParseInt(d, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("clitest: ps time %q", s)
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("clitest: ps time %q", s)
	}
	secs, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, fmt.Errorf("clitest: ps time %q", s)
	}
	total := time.Duration(secs * float64(time.Second))
	for i, mult := len(parts)-2, time.Minute; i >= 0; i, mult = i-1, mult*60 {
		n, err := strconv.ParseInt(parts[i], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("clitest: ps time %q", s)
		}
		total += time.Duration(n) * mult
	}
	return total + time.Duration(days)*24*time.Hour, nil
}

// Loaded is the machine's one-minute load average, and whether it is at or
// over its CPU count; 0 and false when it cannot be read.
func Loaded() (load float64, busy bool) {
	var raw string
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/loadavg")
		if err != nil {
			return 0, false
		}
		raw = string(data)
	} else {
		out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
		if err != nil {
			return 0, false
		}
		raw = strings.Trim(strings.TrimSpace(string(out)), "{} ")
	}
	f := strings.Fields(raw)
	if len(f) == 0 {
		return 0, false
	}
	l, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, false
	}
	return l, l >= float64(runtime.NumCPU())
}
