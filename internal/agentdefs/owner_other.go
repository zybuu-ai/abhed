//go:build !unix

package agentdefs

import "os"

// rootOwnedNotShared cannot read an owner here, so no managed link is
// followed on Windows; its name is still reserved.
func rootOwnedNotShared(os.FileInfo) bool { return false }
