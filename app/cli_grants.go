package app

import (
	"encoding/json"

	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// cliKeeper keeps the command line's "always for this repository" answers
// to host git's asks, as Abhed Studio keeps them: in the person's own state
// (workspace-grants.json), never the workspace, shared with Studio, listed
// and revoked there. Nothing is kept or offered where the managed
// configuration holds the permissions.
type cliKeeper struct {
	workspace string
	locked    bool
}

// grantOf is the repository grant that could answer the ask: a brokered
// fetch or push asked about under host git's step alone.
func (k cliKeeper) grantOf(tool string, args json.RawMessage, res policy.Result) (bashGrant, bool) {
	if k.locked || k.workspace == "" || tool != "bash" || res.Decision != policy.Ask || res.Step != hostgit.StepHostGit {
		return bashGrant{}, false
	}
	fam := hostgit.RemoteFamily(bashCommandOf(args))
	if fam == "" {
		return bashGrant{}, false
	}
	return bashGrant{Family: fam}, true
}

func (k cliKeeper) Kept(tool string, args json.RawMessage, res policy.Result) (string, bool) {
	g, ok := k.grantOf(tool, args, res)
	if !ok || !stillKept(k.workspace, g) {
		return "", false
	}
	return g.scopeOf(), true
}

func (k cliKeeper) Offer(tool string, args json.RawMessage, res policy.Result) string {
	g, ok := k.grantOf(tool, args, res)
	if !ok {
		return ""
	}
	return hostGitGrantLabel(g.Family)
}

func (k cliKeeper) Keep(tool string, args json.RawMessage, res policy.Result) (string, error) {
	g, ok := k.grantOf(tool, args, res)
	if !ok {
		return "", nil
	}
	return g.scopeOf(), keepGrant(k.workspace, g)
}
