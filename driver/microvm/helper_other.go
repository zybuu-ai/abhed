//go:build !darwin

package microvm

import (
	"fmt"
	"runtime"
)

func probeHelper(string) error {
	return fmt.Errorf("the Virtualization.framework helper backend runs only on macOS; this host runs %s", runtime.GOOS)
}
