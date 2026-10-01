package toolset

import (
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// Suggester is the next-prompt suggester an interactive surface gives its
// loop, or nil when the configuration turns it off or its model cannot be had.
func Suggester(cfg config.Config) *agent.Suggester {
	if !cfg.Suggest.Enabled {
		return nil
	}
	sg := &agent.Suggester{Clean: func(s string) string { return ui.CleanText(s, false) }}
	if name := cfg.Suggest.Model; name != "" {
		a, err := ModelResolver(cfg)(name)
		if err != nil {
			return nil
		}
		sg.Adapter = a
	}
	return sg
}
