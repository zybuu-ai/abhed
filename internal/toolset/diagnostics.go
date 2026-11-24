package toolset

import (
	"maps"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/lspdiag"
	"github.com/zybuu-ai/abhed/internal/lsprelay"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Diagnostics is the language server diagnostics the agent is given after
// its edits under cfg (tools.diagnostics), or nil when they are off. The
// servers are sandbox.language_servers over lsprelay.DefaultServers.
func Diagnostics(cfg config.Config, reg *tools.Registry) agent.Diagnoser {
	servers := maps.Clone(lsprelay.DefaultServers)
	maps.Copy(servers, cfg.Sandbox.LanguageServers)
	d := lspdiag.New(lspdiag.Options{
		Mode:          orAuto(cfg.Tools.Diagnostics),
		Servers:       servers,
		Max:           cfg.Tools.DiagnosticsMax,
		Wait:          time.Duration(cfg.Tools.DiagnosticsWaitMS) * time.Millisecond,
		ToolchainRoot: func() string { return sandboxconfig.ToolchainCache(cfg) },
		Find:          ProgramFinder(reg),
	})
	if d == nil {
		return nil
	}
	return d
}

func orAuto(mode string) string {
	if mode == "" {
		return lspdiag.ModeAuto
	}
	return mode
}
