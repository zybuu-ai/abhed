package policy

import (
	"strings"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// guarded sets r's Guard when it offers a scope on a git command: git runs
// what the repository's config and hooks name, which can change after the grant.
func (e *Engine) guarded(r Result) Result {
	if !strings.HasPrefix(r.Scope, "bash(git ") || e.Roots == nil {
		return r
	}
	r.Guard = sandbox.GitFingerprint(e.Roots()...)
	return r
}
