//go:build !linux

package sandbox

import (
	"context"
	"os"
	"os/exec"

	"github.com/zybuu-ai/abhed/internal/fence/probe"
)

// fenceHost holds nothing off Linux, where the fence is never qualified.
type fenceHost struct{}

func (h *fenceHost) path() string { return "" }

func (h *fenceHost) close() error { return nil }

func (h *fenceHost) killAll() error { return nil }

// processGroup is unused off Linux, where no command is fenced.
func processGroup() int { return 0 }

// FenceProbe runs the fence's probe, which reports the fence needs Linux.
func FenceProbe(ctx context.Context, p Policy) probe.Report {
	return probe.Run(ctx, probe.Requirements{AllowNetwork: p.AllowNetwork})
}

// qualifyHost reports the probe's refusal: the fence needs Linux.
func (f *Fence) qualifyHost(ctx context.Context) (bool, string) {
	f.report = FenceProbe(ctx, f.policy)
	return false, f.report.Summary
}

func (f *Fence) wrap(context.Context, string, []string, ...string) *exec.Cmd {
	_, why := f.Available()
	return refusedCmd("%s", why)
}

// folderIdentity is p's identity, not following a link.
func folderIdentity(p string) (folderID, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return folderID{}, err
	}
	return folderID{info: info, dir: info.IsDir()}, nil
}
