package toolset

import (
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/defaults"
	"github.com/zybuu-ai/abhed/internal/ollama"
)

// OllamaTarget is the local Ollama that serves the configured provider
// name, when one does; every surface asks the same question this way.
func OllamaTarget(cfg config.Config, name string) (ollama.Target, bool) {
	p, ok := cfg.Model.Providers[name]
	if !ok {
		return ollama.Target{}, false
	}
	return ollama.Resolve(name, p.Type, p.BaseURL, p.Model, defaults.OllamaURL)
}

// OllamaAssist is what a surface may do for a person who chose an Ollama
// model: the configuration's model.ollama.assist, the machine env, and the
// record each attempt is written to. admin is whether the person may act on
// this host; inAgent says why this process runs inside an agent's command,
// when it does.
func OllamaAssist(cfg config.Config, env ollama.Env, surface string, admin bool, inAgent string, record func(agent.EventType, agent.OllamaAction)) ollama.Assist {
	return ollama.Assist{Env: env, Mode: cfg.OllamaAssist(), Surface: surface, Admin: admin, InAgent: inAgent, Record: record}
}
