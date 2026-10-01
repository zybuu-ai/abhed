package app

import (
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// cliSuggester offers a next prompt in the terminal's input box. Line mode
// has no box to show one in, so it asks for none.
func cliSuggester(cfg config.Config, editor *ui.LineReader) *agent.Suggester {
	if !editor.Raw() {
		return nil
	}
	sg := toolset.Suggester(cfg)
	if sg != nil {
		sg.Hold = editor.Typing // what is being typed is the next prompt
	}
	return sg
}
