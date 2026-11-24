package app

import (
	"context"
	"encoding/json"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// guestRun reports a guest's run, which keeps nothing past its end, so is
// offered no "always".
func guestRun() bool { return currentPrincipal().Kind == identity.Guest }

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
	if k.locked || guestRun() || k.workspace == "" || tool != "bash" || res.Decision != policy.Ask || res.Step != hostgit.StepHostGit {
		return bashGrant{}, false
	}
	return hostGitGrant(hostgit.RemoteFamily(bashCommandOf(args)), res.Scope)
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

// keptApprover answers an ask a kept "always for this repository" grant
// covers, as the terminal's approver does, and puts any other to next: a
// headless run or rpc session uses the grant the person kept, as guide 04
// says, even with nobody to ask.
type keptApprover struct {
	keep cliKeeper
	next agent.Approver
}

func (k keptApprover) Approve(ctx context.Context, tool string, args json.RawMessage, res policy.Result) (bool, error) {
	if scope, ok := k.keep.Kept(tool, args, res); ok {
		agent.NoteAnswer(ctx, agent.Answer{By: agent.ByWorkspaceGrant, Scope: scope})
		return true, nil
	}
	return k.next.Approve(ctx, tool, args, res)
}

// approverFunc is a function as an agent.Approver.
type approverFunc func(context.Context, string, json.RawMessage, policy.Result) (bool, error)

func (f approverFunc) Approve(ctx context.Context, tool string, args json.RawMessage, res policy.Result) (bool, error) {
	return f(ctx, tool, args, res)
}
