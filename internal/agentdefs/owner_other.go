//go:build !unix

package agentdefs

import "os"

// rootOwnedNotShared cannot read an owner here, so no link is followed.
func rootOwnedNotShared(os.FileInfo) bool { return false }
