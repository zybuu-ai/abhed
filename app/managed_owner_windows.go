//go:build windows

package app

import "os"

// fileOwner is not kept on Windows, where the file's ACL decides.
func fileOwner(os.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }

// syncDir is a no-op: Windows cannot open a directory to flush it.
func syncDir(string) error { return nil }
