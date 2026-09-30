//go:build unix

package local

import "syscall"

func setUmask(m int) int { return syscall.Umask(m) }
