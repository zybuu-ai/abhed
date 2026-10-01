//go:build !unix

package ui

import "os"

// ownedByMe cannot tell here; the folder is under the person's own home.
func ownedByMe(os.FileInfo) bool { return true }
