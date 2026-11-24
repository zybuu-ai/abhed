//go:build !unix

package testenv

import "errors"

// checkPrivate cannot read ownership here, so a helper makes a folder of
// its own rather than trust its parent's.
func checkPrivate(string) (string, error) { return "", errors.New("not checked on this system") }

// ownFolderChecked is false: the folder Isolate makes is used as made.
const ownFolderChecked = false
