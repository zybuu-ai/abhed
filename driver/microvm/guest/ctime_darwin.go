package guest

import (
	"io/fs"
	"syscall"
	"time"
)

// changed is when fi's inode last changed; commands cannot set it back.
func changed(fi fs.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Ctimespec.Unix())
	}
	return fi.ModTime()
}
