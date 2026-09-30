package abhed

import (
	"context"
	"fmt"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// Store is where an agent's record is kept: append-only, by session and
// step. Options.Store takes one; without it the record lives in memory and
// ends with the process. OpenLocalRecord opens the durable, chained local
// record the command line uses.
type Store interface {
	Append(Event) error
	Events(sessionID string) ([]Event, error)
	Since(sessionID string, seq int64) ([]Event, error)
}

// OpenLocalRecord opens the local record: dir, or ~/.abhed/records when dir
// is "", for tenant ("" is "default"). Payloads are redacted with the
// secrets store before they are written. The caller closes it after the
// agents that use it.
func OpenLocalRecord(dir, tenant string) (*local.Store, error) {
	red, err := secrets.Default().LoadRedactor()
	if err != nil {
		return nil, fmt.Errorf("abhed: %w", err)
	}
	return local.Open(local.Options{Dir: dir, Tenant: tenant, User: "embedded", Redact: red})
}

// withRecordState makes the local record an agent is given Abhed's state
// for that agent: its real directory is where record.dir points, so the
// file tools, the server's readers and every sandbox tier refuse it, and a
// directory the agent's commands could reach (the workspace, an added
// directory, a temp folder or a cache) is refused outright.
func withRecordState(cfg config.Config, opts Options) (config.Config, error) {
	rec, ok := opts.Store.(*local.Store)
	if !ok {
		return cfg, nil
	}
	cfg.Record.Dir = rec.Dir()
	if err := sandboxconfig.CheckStatePaths(cfg, opts.Workspace); err != nil {
		return cfg, fmt.Errorf("abhed: the record at %s: %w", rec.Dir(), err)
	}
	return cfg, nil
}

// sessionReleaser is a store that holds a writer's lock on a session, as
// the local record does; Close lets it go.
type sessionReleaser interface {
	Release(id string) error
}

// recordFor is the store an agent records to: the one Options.Store names,
// with the session's row created where the store keeps rows, or memory.
func recordFor(ctx context.Context, opts Options, id string, cfg config.Config) (agent.Store, error) {
	if opts.Store == nil {
		return agent.NewMemStore(), nil
	}
	if rec, ok := opts.Store.(interface {
		CreateSession(context.Context, store.SessionRecord) error
	}); ok {
		provider, _ := cfg.Provider()
		if err := rec.CreateSession(ctx, store.SessionRecord{
			ID: id, User: "embedded", Workspace: opts.Workspace, Model: provider.Model,
			Mode: opts.Mode,
		}); err != nil {
			return nil, fmt.Errorf("abhed: start the session's record: %w", err)
		}
	}
	return opts.Store, nil
}

// releaseRecord lets go of the session in a store that holds it, so another
// process may continue it.
func (a *Agent) releaseRecord() {
	if r, ok := a.store.(sessionReleaser); ok {
		_ = r.Release(a.id)
	}
}

// ID is the agent's session id, the one its record is kept under.
func (a *Agent) ID() string { return a.id }
