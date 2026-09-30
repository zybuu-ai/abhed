//go:build darwin

package local

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncFile makes what was written to f durable with fsync. On macOS Go's
// File.Sync asks the drive to flush its own cache too (F_FULLFSYNC), which
// costs several milliseconds a call; plain fsync, as SQLite uses by default
// there, survives a crash of the process or the system, and a power cut may
// lose what the drive had cached.
func syncFile(f *os.File) error {
	for {
		err := unix.Fsync(int(f.Fd()))
		if err != unix.EINTR {
			return err
		}
	}
}
