package app

import (
	"os"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// supervisorDoctor is doctor's supervisor line, with what would make a model
// judge fail closed on every call.
func supervisorDoctor(cfg config.Config) string {
	line := toolset.SupervisorLabel(cfg)
	m := cfg.Monitor
	if m.JudgeName() != "model" {
		return line
	}
	var notes []string
	if m.APIKeyEnv != "" && os.Getenv(m.APIKeyEnv) == "" {
		notes = append(notes, config.Printable(m.APIKeyEnv)+" is not set, so the judge is sent no key; an endpoint that needs one fails every judgement closed")
	}
	if p, ok := cfg.Model.Providers[cfg.Model.Default]; ok && p.Model != "" && p.Model == m.Model {
		notes = append(notes, "the judge is the agent's own model; one of another family is less likely to share its mistakes")
	}
	for _, n := range notes {
		line += "\n            ⚠ " + n
	}
	return strings.TrimSpace(line)
}
