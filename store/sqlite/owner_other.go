//go:build !unix

package sqlite

import "os"

// Modes carry no owner or group bits here; the folder's ACL decides.
const windows = true

func ownedByMe(os.FileInfo) error { return nil }
