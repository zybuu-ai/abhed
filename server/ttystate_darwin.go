//go:build darwin

package server

import "golang.org/x/sys/unix"

const getTermios = unix.TIOCGETA
