//go:build !linux

package probe

import (
	"context"
	"runtime"
	"time"
)

// Run reports that the fence is not supported on this OS.
func Run(_ context.Context, _ Requirements) Report {
	r := Report{OS: runtime.GOOS, Arch: runtime.GOARCH, Checked: time.Now().UTC()}
	r.Checks = []Check{{ID: CheckPlatform, Status: Fail, Required: true,
		Reason: "the fence is not supported on this OS (" + runtime.GOOS + "); it needs Linux", Value: runtime.GOOS + "/" + runtime.GOARCH}}
	r.finish()
	return r
}
