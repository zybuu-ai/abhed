//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd

package ollama

import "errors"

// diskFree is not read here; the disk check is then skipped and said to be.
func diskFree(string) (uint64, error) { return 0, errors.New("free space is not known on this system") }
