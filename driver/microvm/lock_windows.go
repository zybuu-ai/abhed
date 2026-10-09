package microvm

import (
	"errors"
	"os"
)

func lockDir(string) (func(), error) {
	return nil, errors.New("the microvm driver does not run on Windows")
}

func ownedByMe(os.FileInfo) bool { return false }
