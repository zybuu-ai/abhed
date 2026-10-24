//go:build darwin

package ollama

import "golang.org/x/sys/unix"

// memTotal is the machine's physical memory.
func memTotal() (uint64, error) { return unix.SysctlUint64("hw.memsize") }
