//go:build windows

package nlink

import (
	"os"
	"syscall"
)

// Of returns 1: Windows file information carries no link count; Linked
// reads it from the file itself.
func Of(os.FileInfo) uint64 { return 1 }

// pathCount reads the file's link count from its handle, or 1 when it
// cannot be opened or read.
func pathCount(path string, _ os.FileInfo) uint64 {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 1
	}
	h, err := syscall.CreateFile(p, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 1
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &d); err != nil {
		return 1
	}
	return uint64(d.NumberOfLinks)
}
