// Package sandboxconfig builds the sandbox a configuration asks for, so the
// command line and the SDK cannot read sandbox settings differently.
package sandboxconfig

import (
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// Build selects an execution backend meeting the configured minimum tier.
// Select never silently downgrades, so an error is a configuration problem.
func Build(cfg config.Config, workspace string) (sandbox.Sandbox, error) {
	p := sandbox.DefaultPolicy(workspace)
	if cfg.Sandbox.MinTier != "" {
		p.MinTier = sandbox.Tier(cfg.Sandbox.MinTier)
	}
	p.AllowNetwork = cfg.Sandbox.AllowNetwork
	p.ReadOnlyPaths = cfg.Sandbox.ReadOnlyPaths
	if cfg.Sandbox.MaxMemoryMB > 0 {
		p.MaxMemoryMB = cfg.Sandbox.MaxMemoryMB
	}
	if cfg.Sandbox.MaxProcs > 0 {
		p.MaxProcs = cfg.Sandbox.MaxProcs
	}
	return sandbox.Select(p)
}
