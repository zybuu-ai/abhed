//go:build linux

package server

import "golang.org/x/sys/unix"

const getTermios = unix.TCGETS
