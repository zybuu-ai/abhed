//go:build !darwin && !linux

package guest

import (
	"io/fs"
	"time"
)

func changed(fi fs.FileInfo) time.Time { return fi.ModTime() }
