//go:build linux

package termline

import "golang.org/x/sys/unix"

const getTermios = unix.TCGETS
