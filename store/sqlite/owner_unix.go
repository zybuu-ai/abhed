//go:build unix

package sqlite

import (
	"fmt"
	"os"
	"syscall"
)

const windows = false

// ownedByMe refuses a file or folder another user owns.
func ownedByMe(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("it belongs to another user (uid %d)", st.Uid)
	}
	return nil
}
