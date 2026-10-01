// Package embedded lets this module's own surfaces, such as the editor
// protocol, reach what an SDK agent is built from without widening the SDK's
// public API. Nothing outside the module can import it.
package embedded

import (
	"context"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// Settings are what a surface in this module may ask of the SDK's New beyond
// its Options, carried on the context New is given.
type Settings struct {
	// ID names the session; "" lets New choose one.
	ID string
	// Resume continues session ID, already in the store, instead of starting it.
	Resume bool
	// User is who a new session is recorded for; "" is "embedded".
	User string
	// Protect are workspace paths the agent's commands may read but not write.
	Protect []string
	// ProtectGit keeps commands from writing any git folder's config and
	// hooks in the workspace, at any depth, where the sandbox can name them.
	ProtectGit bool
	// Surface names the entry point (acp, rpc); a new session records it in
	// its session.started, with the mode it runs in.
	Surface string
}

type settingsKey struct{}

// With returns ctx carrying s for New.
func With(ctx context.Context, s Settings) context.Context {
	return context.WithValue(ctx, settingsKey{}, s)
}

// From returns the settings ctx carries, the zero value when none.
func From(ctx context.Context) Settings {
	s, _ := ctx.Value(settingsKey{}).(Settings)
	return s
}

// Parts are an SDK agent's insides.
type Parts struct {
	ID      string
	Loop    *agent.Loop
	Session *tools.Session
	Config  config.Config
	Set     *toolset.Set
	// Store is the store the session is recorded in, as Options.Store gave it.
	Store agent.Store
}

// Of returns an SDK agent's parts; the SDK sets it. ok is false for anything
// that is not an SDK agent, such as a test's stand-in.
var Of = func(any) (Parts, bool) { return Parts{}, false }
