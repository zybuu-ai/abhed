//go:build darwin

package app

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// hiddenEnvFD names the descriptor a re-executed Abhed reads its hidden
// variables from. It holds a number, never a value.
const hiddenEnvFD = "ABHED_HIDDEN_ENV_FD"

// maxHiddenEnv bounds what goes through the pipe, so the write before exec
// never waits on a reader: macOS pipes take 16 KiB without one.
const maxHiddenEnv = 16 << 10

// shownEnv are the variables left in Abhed's exec environment, which any
// process of the same user can read with kern.procargs2. None holds a secret.
var shownEnv = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true, "PWD": true,
	"TERM": true, "TERM_PROGRAM": true, "COLORTERM": true, "LANG": true, "TZ": true, "NO_COLOR": true,
	"__CF_USER_TEXT_ENCODING": true,
}

func shownVar(kv string) bool {
	k, _, _ := strings.Cut(kv, "=")
	return shownEnv[k] || strings.HasPrefix(k, "LC_")
}

// HideEnvironment keeps Abhed's environment out of kern.procargs2, which
// Seatbelt cannot refuse: it re-executes Abhed with only shownEnv in its exec
// environment and hands the rest over a pipe, back into Go's environment.
// The process keeps its id. A program's main calls it first; it returns at
// once when there is nothing to hide, or when the re-exec cannot be made.
func HideEnvironment() {
	if v, ok := os.LookupEnv(hiddenEnvFD); ok {
		restoreHidden(v)
		return
	}
	if os.Getenv("ABHED_SHOW_ENV") == "1" {
		return
	}
	var keep, hide []string
	for _, kv := range os.Environ() {
		if shownVar(kv) {
			keep = append(keep, kv)
		} else {
			hide = append(hide, kv)
		}
	}
	block := strings.Join(hide, "\x00")
	if len(hide) == 0 || len(block) > maxHiddenEnv || strings.Contains(block, hiddenEnvFD+"=") {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	r, w, err := os.Pipe()
	if err != nil {
		return
	}
	defer func() { _ = r.Close() }()
	_, err = w.Write([]byte(block))
	if cerr := w.Close(); err != nil || cerr != nil {
		return
	}
	fd := int(r.Fd())
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, 0); errno != 0 {
		return
	}
	err = syscall.Exec(exe, os.Args, append(keep, hiddenEnvFD+"="+strconv.Itoa(fd))) // #nosec G204 -- this same program
	// Still here: the exec failed, and the environment is as it was.
	fmt.Fprintf(os.Stderr, "abhed: warning: could not hide the environment from other processes: %v\n", err)
}

// restoreHidden reads the hidden variables back from descriptor v, into Go's
// environment only: the exec environment the kernel shows stays without them.
func restoreHidden(v string) {
	_ = os.Unsetenv(hiddenEnvFD)
	fd, err := strconv.Atoi(v)
	if err != nil || fd < 3 {
		return
	}
	f := os.NewFile(uintptr(fd), "hidden-env")
	defer func() { _ = f.Close() }()
	buf := make([]byte, maxHiddenEnv+1)
	n := 0
	for n < len(buf) {
		m, err := f.Read(buf[n:])
		n += m
		if err != nil || m == 0 {
			break
		}
	}
	for _, kv := range strings.Split(string(buf[:n]), "\x00") {
		if k, val, ok := strings.Cut(kv, "="); ok && k != "" {
			_ = os.Setenv(k, val)
		}
	}
}
