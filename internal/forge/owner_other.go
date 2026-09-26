//go:build !unix

package forge

import "os"

// fileOwnedByMe has no owner to compare on this system; the folder's other
// checks still apply.
func fileOwnedByMe(os.FileInfo) bool { return true }
