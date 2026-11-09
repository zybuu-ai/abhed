//go:build linux

package termline

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const getTermios = unix.TCGETS

// Process is pid's command name as the kernel keeps it, its parent and its
// process group; false if no live process has that number.
func Process(pid int) (name string, ppid, pgid int, ok bool) {
	if pid <= 0 {
		return "", 0, 0, false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") // #nosec G304 -- a fixed /proc path for a number
	if err != nil {
		return "", 0, 0, false
	}
	s := string(b)
	open, closing := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || closing < open {
		return "", 0, 0, false
	}
	// After the name: state, ppid, pgrp.
	f := strings.Fields(s[closing+1:])
	if len(f) < 3 || f[0] == "Z" || f[0] == "X" {
		return "", 0, 0, false
	}
	ppid, err1 := strconv.Atoi(f[1])
	pgid, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil {
		return "", 0, 0, false
	}
	return s[open+1 : closing], ppid, pgid, true
}
