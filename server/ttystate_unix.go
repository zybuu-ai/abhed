//go:build darwin || linux

package server

import (
	"os"

	"golang.org/x/sys/unix"
)

// ttyNow reads what the terminal itself says, from the master side: which
// process group has the foreground, and whether the line discipline is in
// canonical mode with echo off, which is how a program asks for a password.
func ttyNow(master *os.File) (fg int, secret bool, ok bool) {
	fd := int(master.Fd()) // #nosec G115 -- a file descriptor fits an int
	fg, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil {
		return 0, false, false
	}
	t, err := unix.IoctlGetTermios(fd, getTermios)
	if err != nil {
		return 0, false, false
	}
	return fg, t.Lflag&unix.ICANON != 0 && t.Lflag&unix.ECHO == 0, true
}
