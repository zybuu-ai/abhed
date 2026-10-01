//go:build darwin

package termline

import "golang.org/x/sys/unix"

const getTermios = unix.TIOCGETA
