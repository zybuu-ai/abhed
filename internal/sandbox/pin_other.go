//go:build !unix

package sandbox

import (
	"errors"
	"os"
)

// openPinned holds nothing here: on Windows an open file cannot be renamed or
// removed, which would lock the script for the whole session, so the pin is
// its stat alone.
func openPinned(string) (*os.File, error) {
	return nil, errors.New("a pinned file is not held open here")
}
