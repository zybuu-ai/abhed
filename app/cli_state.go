package app

import (
	"sync/atomic"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/server"
)

// cliState carries what the slash commands need across turns.
type cliState struct {
	store     server.EventStore
	appCfg    config.Config
	loop      *agent.Loop
	sessionID string
	total     agent.Usage
	undo      *agent.UndoLog
	workspace string
	adapter   model.Adapter
	provider  config.ProviderConfig
	// open starts the loop for session id and makes it the conversation.
	open func(id string) *agent.Loop
	// claim is a resumed session its first task must claim before it runs,
	// and claimSeq the last seq of the record it was rebuilt from.
	claim    string
	claimSeq int64
	// moved is the switch a resumed session makes by continuing on another
	// model, recorded when its first task has claimed it.
	moved *agent.ModelSwitched
	// sess is the tool session, whose undo log a new conversation replaces.
	sess *tools.Session
	// transcript accumulates the session for /export.
	transcript []agent.Event
	// scopes are the "always allow" answers, which end with the session.
	scopes *ui.AllowList
	// unfollow ends the conversation's subscription; rendered is the last
	// seq it drew.
	unfollow func()
	rendered atomic.Int64
	// dynamic are the run-time slash command sources, looked up after the
	// built-ins; see slashSource.
	dynamic []slashSource
}

// follow draws the conversation's events as they are recorded, for as long as
// it is open: a background result arrives at the prompt as well as in a task.
func (c *cliState) follow(store server.EventStore, id string, r *ui.Renderer) {
	events := store.Subscribe(id)
	c.rendered.Store(0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			r.Event(ev)
			c.rendered.Store(ev.Seq)
		}
	}()
	c.unfollow = func() {
		store.Unsubscribe(id, events)
		<-done
	}
}

// waitRendered waits a moment for the events up to seq to be drawn, so a
// task's usage prints after its output.
func (c *cliState) waitRendered(seq int64) {
	for deadline := time.Now().Add(time.Second); c.rendered.Load() < seq && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
}

// fresh forgets the last conversation's cost, transcript, undo log, allowed
// scopes, logins and connected hosts, for a new or resumed one; the workspace
// is left as it is.
func (c *cliState) fresh() {
	c.total, c.transcript, c.claim, c.moved = agent.Usage{}, nil, "", nil
	if c.scopes != nil {
		c.scopes.Reset()
	}
	if c.sess != nil {
		c.undo = agent.NewUndoLog(c.sess.RestoreFile, c.sess.RemoveFile)
		c.sess.Checkpoint = c.undo.Record
		// Logins and connected hosts belong to the conversation that made them.
		c.sess.ResetScoped()
	}
}

// usageSince is what a conversation spent between two readings of its usage.
func usageSince(before, after agent.Usage) agent.Usage {
	return agent.Usage{
		InputTokens:       after.InputTokens - before.InputTokens,
		OutputTokens:      after.OutputTokens - before.OutputTokens,
		CachedTokens:      after.CachedTokens - before.CachedTokens,
		ColdPrefillTokens: after.ColdPrefillTokens - before.ColdPrefillTokens,
		Turns:             after.Turns - before.Turns,
		Compactions:       after.Compactions - before.Compactions,
	}
}

func (c *cliState) accumulate(u agent.Usage) {
	c.total.InputTokens += u.InputTokens
	c.total.OutputTokens += u.OutputTokens
	c.total.CachedTokens += u.CachedTokens
	c.total.ColdPrefillTokens += u.ColdPrefillTokens
	c.total.Turns += u.Turns
	c.total.Compactions += u.Compactions
}
