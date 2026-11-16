//go:build !unix

package bgcli

import (
	"errors"
	"os"
)

// readNoFollow is not served here: a snapshot is read without following
// links, which this system's calls do not give.
func readNoFollow(*os.File, string) ([]byte, os.FileInfo, string, error) {
	return nil, nil, "", errors.New("a snapshot is sent from Linux and macOS only; pass -no-snapshot")
}
