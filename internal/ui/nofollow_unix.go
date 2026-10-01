//go:build unix

package ui

import "syscall"

// noFollow refuses to open a file through a symbolic link.
const noFollow = syscall.O_NOFOLLOW
