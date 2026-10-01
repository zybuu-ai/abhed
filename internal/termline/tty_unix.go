//go:build darwin || linux

package termline

import (
	"os"

	"golang.org/x/sys/unix"
)

// TTYNow reads what the terminal itself says, from the master side: which
// process group has the foreground, and whether the line discipline is in
// canonical mode. At its prompt bash reads keys itself, in non-canonical mode;
// in canonical mode something else is reading a line, a password perhaps.
// It goes through SyscallConn, since Fd would switch the master to blocking
// reads and a Close could then no longer interrupt the pump.
func TTYNow(master *os.File) (fg int, canonical bool, ok bool) {
	rc, err := master.SyscallConn()
	if err != nil {
		return 0, false, false
	}
	var t *unix.Termios
	var ferr, terr error
	if err := rc.Control(func(fd uintptr) {
		fg, ferr = unix.IoctlGetInt(int(fd), unix.TIOCGPGRP) // #nosec G115 -- a file descriptor fits an int
		t, terr = unix.IoctlGetTermios(int(fd), getTermios)  // #nosec G115 -- a file descriptor fits an int
	}); err != nil || ferr != nil || terr != nil {
		return 0, false, false
	}
	return fg, t.Lflag&unix.ICANON != 0, true
}

// Hidden reports whether something is reading a line the terminal does not
// show: canonical mode with echo off, as read -s and password prompts set.
func Hidden(master *os.File) bool {
	rc, err := master.SyscallConn()
	if err != nil {
		return false
	}
	var t *unix.Termios
	var terr error
	if err := rc.Control(func(fd uintptr) {
		t, terr = unix.IoctlGetTermios(int(fd), getTermios) // #nosec G115 -- a file descriptor fits an int
	}); err != nil || terr != nil {
		return false
	}
	return t.Lflag&unix.ICANON != 0 && t.Lflag&unix.ECHO == 0
}
