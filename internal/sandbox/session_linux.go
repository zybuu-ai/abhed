//go:build linux

package sandbox

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// EndSession kills every process still in session sid: what a shell started
// and left behind, however it was detached from the shell's job table. A
// process that made a session of its own (setsid) is not in it.
func EndSession(sid int) {
	if sid <= 1 {
		return
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat") // #nosec G304 -- a numbered /proc entry
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp session ...; comm may hold spaces.
		s := string(stat)
		f := strings.Fields(s[strings.LastIndexByte(s, 0x29)+1:])
		if len(f) > 3 && f[3] == strconv.Itoa(sid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
