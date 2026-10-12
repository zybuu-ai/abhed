//go:build unix

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// stateUseLock takes a shared lock marking a fence over ws's .abhed; only a
// fence holding it alone may remove the folder it made.
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

// processAlive reports whether pid names a running process.
func processAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// madeKey names folder id for the state lock: device, inode and birth.
func madeKey(id folderID) string {
	st, ok := id.info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("made %d %d %d %v", st.Dev, st.Ino, id.birth, id.hasBirth)
}

// noteMade records in the state lock that a fence made the folder id.
func noteMade(l *os.File, id folderID) {
	if k := madeKey(id); l != nil && k != "" {
		_ = l.Truncate(0)
		_, _ = l.WriteAt([]byte(k), 0)
	}
}

// madeNoted reports whether the state lock records that a fence made id,
// clearing the record.
func madeNoted(l *os.File, id folderID) bool {
	if l == nil {
		return false
	}
	b, _ := io.ReadAll(io.NewSectionReader(l, 0, 256))
	_ = l.Truncate(0)
	k := madeKey(id)
	return k != "" && string(b) == k
}
