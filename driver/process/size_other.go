//go:build !linux

package process

import (
	"runtime"

	"github.com/zybuu-ai/abhed/driver"
)

// hostSizer sizes nothing: only Linux has the cgroups a size needs.
var hostSizer sizing = noSizer{}

type noSizer struct{}

func (noSizer) Check() error {
	return driver.Errorf(driver.ErrInvalid, "the process driver cannot bound a sandbox's cpus or memory on %s, which has no cgroups; "+
		"use the container or microvm driver, or leave the size unset", runtime.GOOS)
}

func (n noSizer) NewBox(string, size) (box, error) { return nil, n.Check() }
