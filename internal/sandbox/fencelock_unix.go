//go:build unix

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// stateUseLock takes a shared lock that says a fence on ws mounts over its
// .abhed; nil when none can be taken. Another fence that holds it alone may
// remove the folder it made.
func stateUseLock(ws string) *os.File {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dir := filepath.Join(home, stateDir, "fence")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	sum := sha256.Sum256([]byte(RealPath(ws)))
	f, err := os.OpenFile(filepath.Join(dir, hex.EncodeToString(sum[:8])+".lock"), os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- under ~/.abhed
	if err != nil {
		return nil
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH); err != nil { // #nosec G115 -- a descriptor
		_ = f.Close()
		return nil
	}
	return f
}

// lastUser reports whether the shared lock l can be made exclusive at once:
// no other fence on the workspace holds it.
func lastUser(l *os.File) bool {
	return l != nil && unix.Flock(int(l.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil // #nosec G115 -- a descriptor
}
