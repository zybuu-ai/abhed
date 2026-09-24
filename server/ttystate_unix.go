//go:build darwin || linux

package server

import (
	"os"

	"golang.org/x/sys/unix"
)

// ttyNow reads what the terminal itself says, from the master side: which
// process group has the foreground, and whether the line discipline is in
// canonical mode with echo off, which is how a program asks for a password.
// It goes through SyscallConn, since Fd would switch the master to blocking
// reads and a Close could then no longer interrupt the pump.
func ttyNow(master *os.File) (fg int, secret bool, ok bool) {
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
	return fg, t.Lflag&unix.ICANON != 0 && t.Lflag&unix.ECHO == 0, true
}
