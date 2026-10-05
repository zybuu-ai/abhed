package app

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
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
	// open starts the loop for session id, whose record goes on after seq
	// after (0 for a new one), and makes it the conversation.
	open func(id string, after int64) *agent.Loop
	// startedID is the conversation whose session.started this process
	// recorded; startOwed is the seq a continued one's start goes after, 0 for none.
	startedID string
	startOwed int64
	// recordStart records how this process started, into rec; nil records nothing.
	recordStart func(rec *agent.Recorder, resumedAfter int64)
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
	// panel lists the conversation's subagents and background jobs.
	panel *workPanel
	// surface is the session's ui.Surface, once the terminal UI provides one.
	surface ui.Surface
	// surfaceReadsLines is set while surface is the prompt's line surface,
	// which answers from the typed lines the steering loop reads during a run.
	surfaceReadsLines bool
	// input is what the input layer keeps across lines; see inputState.
	input inputState
	// pending are the person's actions made before a conversation had a
	// record, recorded when the next one opens; see recordCLI.
	pending []pendingEvent
	// overlay is the session's own permission rules, which a new
	// conversation starts without.
	overlay *policy.Overlay
	// turnLimit is the loop's configured turn limit: per message, or for the
	// whole conversation under a managed one; see turnsPerMessage.
	turnLimit int
	// hooks are the session's extensions; hookRecorder is the record of the
	// open conversation, where a hook that fires is recorded.
	hooks        *extension.Host
	hookRecorder atomic.Pointer[agent.Recorder]
	// pol is the session's engine; addedDirs are the directories /add-dir
	// added. A new conversation's record restates both; see carryState.
	pol       *policy.Engine
	addedDirs []agent.WorkspaceDirAdded
	// pendingName is a name given before the conversation exists.
	pendingName string
	// copiedID is the session a record from elsewhere, or one that failed
	// verification, was copied into, with the events copied.
	copiedID     string
	copiedEvents []agent.Event
	// sandbox is the session's sandbox, chosen behind the prompt.
	sandbox *lazySandbox
	// version is the binary's, for /release-notes and /bug.
	version string
	// set and registry are the session's tools, for the panels.
	set      *toolset.Set
	registry *tools.Registry
	// statuslineWarned is set once a failing statusline command was named;
	// statuslineSlow once a first run that ran out of time went unsaid.
	statuslineWarned bool
	statuslineSlow   bool
	// statuslineSB is the statusline's own sandbox, chosen at its first run.
	// statuslineRoots are the granted folders it was judged against; a
	// change to them judges it again. statuslineMu guards all of these.
	statuslineMu     sync.Mutex
	statuslineJudged bool
	statuslineRoots  []string
	statuslineSB     sandbox.Sandbox
	statuslineErr    error
	// statuslineCmd is what runs, and statuslinePin the script it names,
	// checked again before each run.
	statuslineCmd string
	statuslinePin sandbox.ReadableFile
	// checkpoint wraps the undo log's hook, so the renderer sees each file
	// as it was before a change and can draw its diff.
	checkpoint func(next func(string, []byte, bool)) func(string, []byte, bool)
}

// follow draws the conversation's events as they are recorded, for as long as
// it is open: a background result arrives at the prompt as well as in a task.
// recorded is the last seq written so far; those events never reach the
// subscription, so they count as drawn and no turn waits on them.
func (c *cliState) follow(store server.EventStore, id string, r *ui.Renderer, recorded func() int64) {
	events := store.Subscribe(id)
	c.rendered.Store(recorded())
	done := make(chan struct{})
	go func() {
		defer ui.RestoreOnPanic() // a panic drawing an event must not leave the terminal raw
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

// waitDrawn waits a moment for what the conversation has recorded so far to
// be drawn: the footer reads the model and tokens from what was drawn, so a
// command's own events, such as a model switch, are in it before it refreshes.
func (c *cliState) waitDrawn() {
	if c.unfollow != nil && c.loop != nil {
		c.waitRendered(c.loop.Recorder.LastAppended())
	}
}

// waitRendered waits a moment for the events up to seq to be drawn, so a
// task's usage prints after its output.
func (c *cliState) waitRendered(seq int64) {
	deadline := time.Now().Add(renderWait)
	last, moved := c.rendered.Load(), time.Now()
	for last < seq && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		// Drawing that has stopped moving will not catch up: a backlog of
		// queued commands each waited the whole second.
		if now := c.rendered.Load(); now != last {
			last, moved = now, time.Now()
		} else if time.Since(moved) > renderStall {
			return
		}
	}
}

// renderWait bounds a wait for drawing; renderStall ends it early once
// drawing has made no progress for that long.
const (
	renderWait  = time.Second
	renderStall = 100 * time.Millisecond
)

// fresh forgets the last conversation's cost, transcript, undo log, allowed
// scopes, logins and connected hosts, for a new or resumed one; the workspace
// is left as it is.
func (c *cliState) fresh() {
	c.total, c.transcript, c.claim, c.moved = agent.Usage{}, nil, "", nil
	if c.scopes != nil {
		c.scopes.Reset()
	}
	// Rules added "for this session" end with it, and so does the record of
	// adding one that no conversation has held yet.
	c.overlay.Clear()
	c.dropPending(agent.EvPermissionChanged)
	c.carryState()
	if c.sess != nil {
		c.undo = agent.NewUndoLog(c.sess.RestoreFile, c.sess.RemoveFile)
		c.undo.Persist = checkpointSaver(c)
		c.sess.Checkpoint = c.undo.Record
		if c.checkpoint != nil {
			c.sess.Checkpoint = c.checkpoint(c.undo.Record)
		}
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
