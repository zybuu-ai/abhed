//go:build unix

package nlink

import (
	"os"
	"syscall"
)

// Of returns how many names the file has, or 1 when that cannot be read.
func Of(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:unconvert // the field's type differs by platform
	}
	return 1
}

// pathCount is Of for the file at path.
func pathCount(_ string, info os.FileInfo) uint64 { return Of(info) }

// OfFile returns how many names the open file has, from its own fstat.
func OfFile(f *os.File) uint64 {
	info, err := f.Stat()
	if err != nil {
		return 1
	}
	return Of(info)
}
