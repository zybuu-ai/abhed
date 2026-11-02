//go:build !darwin && !linux

package ollama

import "errors"

var errUnknown = errors.New("not known on this system")

// memTotal is not read here; the memory check is then skipped and said to be.
func memTotal() (uint64, error) { return 0, errUnknown }
