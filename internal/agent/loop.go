package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/monitor"
)

// ErrShutdown, given as a context cancel cause, marks a session ended by the
// server stopping rather than by the user interrupting.
var ErrShutdown = errors.New("server shutdown")

// terminalForCancel distinguishes the two ways a run is cancelled. The audit
// log has to tell "someone stopped this" from "the process went away".
func terminalForCancel(ctx context.Context) TerminalReason {
	if errors.Is(context.Cause(ctx), ErrShutdown) {
		return TermShutdown
	}
	return TermUserInterrupt
}

// Approver decides on a tool call that policy routed to Ask. Returning false
// feeds a denial back to the model so it can adapt rather than retry.
type Approver interface {
	Approve(ctx context.Context, tool string, args json.RawMessage, res policy.Result) (bool, error)
}

type requestIDKey struct{}

// RequestIDOf reports the id of the action.requested event an Approver is
// asked about. Unlike a model's call id it is unique, so an answer that names
// it cannot be taken for the answer to a later request.
func RequestIDOf(ctx context.Context) string { id, _ := ctx.Value(requestIDKey{}).(string); return id }

// WithRequestID names the action.requested event an Approver is asked about.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// AutoApprove is for headless runs and tests where policy alone decides.
type AutoApprove struct{ Yes bool }

func (a AutoApprove) Approve(context.Context, string, json.RawMessage, policy.Result) (bool, error) {
	return a.Yes, nil
}

type Config struct {
	MaxTurns  int
	MaxTokens int
	CompactAt float64 // fraction of the context window
	// OffloadAt is the fraction of the window at which old tool results move
	// out to the record. Zero turns offloading off.
	OffloadAt    float64
	SystemPrompt string
	Temperature  *float64
	Effort       model.EffortLevel
}

func DefaultConfig() Config {
	return Config{MaxTurns: 100, MaxTokens: 8192, CompactAt: 0.90, OffloadAt: 0.60}
}

// Loop is the agent's execution loop: a turn-based cycle that terminates
// normally on a response with no tool calls, and abnormally through roughly
// ten other exits — each a distinct, logged terminal event (docs §02).
type Loop struct {
	Adapter   model.Adapter
	Tools     *tools.Registry
	Policy    *policy.Engine
	Approver  Approver
	Session   *tools.Session
	Recorder  *Recorder
	Config    Config
	Compactor *Compactor
	// Offloader moves old tool results out of the window and into the
	// record before compaction is needed. Nil leaves the window alone.
	Offloader *Offloader
	// Monitor, when set, judges each call policy would allow or ask about
	// against the remit and the agent's reasoning, and may only tighten the
	// decision. Nil consults nobody.
	Monitor *monitor.Guard
	// reasoning and recentCalls are the monitor's short memory: the agent's
	// last few stated thoughts, and the last few calls with their outcomes.
	// Calls in one turn run concurrently, so both sit behind monitorMu.
	reasoning   []string
	recentCalls []monitor.Recent
	monitorMu   sync.Mutex

	// Budget caps total token spend across the parent and its subagents.
	// Nil means no cap.
	Budget *Budget

	messages []model.Message
	usage    Usage
	turns    int

	// repeatedFailures counts consecutive identical tool calls that returned an
	// error. A model that ignores an error message and retries verbatim will
	// otherwise burn the entire turn budget on one mistake.
	//
	// Guarded because independent tool calls in one turn run concurrently, and
	// concurrent writes to a Go map are not a race that corrupts a counter —
	// they abort the process.
	repeatedFailures map[string]int
	failuresMu       sync.Mutex

	// recordErr is the first failure to write an event. The record is the
	// session: a run that continued past a store that stopped taking events
	// would leave a replay that ends before the run did. So the failure is
	// kept here and ends the run at the next turn boundary, which is the
	// first point where ending it leaves the record consistent.
	recordErr error
	recordMu  sync.Mutex

	// dropEffort is set once a turn has spent its whole output budget on
	// reasoning without acting; later calls ask for low effort, where the
	// provider offers the choice, so the next turn reaches a tool call.
	dropEffort bool

	// emptyTurns counts consecutive turns that produced neither text nor a
	// tool call, so a model that stalls is nudged rather than mistaken for one
	// that finished.
	emptyTurns int

	// todos is the agent's task list, recorded whenever it changes so a replay
	// shows what the plan was believed to be at each point.
	todos []Todo

	// steer carries messages sent while the agent is working. Reading them at
	// a turn boundary is what lets a user redirect a run instead of killing it.
	steer   []QueuedMessage
	steerMu sync.Mutex
}

// QueuedMessage is a message waiting for the next turn boundary. Its ID is
// carried on the user.message that delivers it, so a client can match the two.
type QueuedMessage struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// ClientID is the sender's own id for the message, when it gave one.
	ClientID string    `json:"client_id,omitempty"`
	At       time.Time `json:"queued_at"`
}

// Steer delivers a message to a running agent, applied at the next turn
// boundary.
//
// Without it the only way to correct an agent that has misunderstood is to
// interrupt and start again, which discards everything it has already learned —
// the reading, the tool results, the half-built context. The user pays for that
// work twice and usually re-types the request. A steering message costs one
// turn and keeps all of it.
//
// It lands between turns rather than mid-turn on purpose: a tool call already
// in flight finishes and its result is recorded, so the transcript never shows
// a call with no outcome.
func (l *Loop) Steer(text string) { l.Queue(text) }

// Queue is Steer that returns the message's id, or "" for a blank message.
func (l *Loop) Queue(text string) string { return l.QueueMessage(Message{Text: text}) }

// QueueMessage queues m, keeping its ClientID for the user.message that
// delivers it. Its QueueID is assigned here.
func (l *Loop) QueueMessage(m Message) string {
	if strings.TrimSpace(m.Text) == "" {
		return ""
	}
	q := QueuedMessage{ID: "q_" + newID(), Text: m.Text, ClientID: m.ClientID, At: time.Now().UTC()}
	l.steerMu.Lock()
	defer l.steerMu.Unlock()
	l.steer = append(l.steer, q)
	return q.ID
}

// Unqueue withdraws a message that has not been delivered yet. It reports
// false once the loop has taken it, since the model may already have read it.
func (l *Loop) Unqueue(id string) bool {
	l.steerMu.Lock()
	defer l.steerMu.Unlock()
	for i, q := range l.steer {
		if q.ID == id {
			l.steer = append(l.steer[:i:i], l.steer[i+1:]...)
			return true
		}
	}
	return false
}

// Queued returns the messages still waiting, oldest first.
func (l *Loop) Queued() []QueuedMessage {
	l.steerMu.Lock()
	defer l.steerMu.Unlock()
	return append([]QueuedMessage(nil), l.steer...)
}

// takeSteering removes and returns any pending steering messages.
func (l *Loop) takeSteering() []QueuedMessage {
	l.steerMu.Lock()
	defer l.steerMu.Unlock()
	if len(l.steer) == 0 {
		return nil
	}
	out := l.steer
	l.steer = nil
	return out
}

// deliverQueued records and applies every waiting message, oldest first.
func (l *Loop) deliverQueued() error {
	for _, q := range l.takeSteering() {
		// A steer that cannot be recorded is not applied: the record is the
		// session, and a message the model saw but the log did not would
		// make a replay diverge from what happened.
		if _, err := l.Recorder.Record(EvUserMessage, ActorUser, Trusted, Message{Text: q.Text, QueueID: q.ID, ClientID: q.ClientID}); err != nil {
			return err
		}
		l.messages = append(l.messages, model.Message{Role: model.RoleUser, Content: q.Text})
	}
	return nil
}

// Todos returns the current task list.
func (l *Loop) Todos() []Todo { return l.todos }

// RecordTodos stores a new list and emits the event. It is exported so the
// todo tool can report through the loop rather than carrying a recorder.
func (l *Loop) RecordTodos(items []Todo, note string) {
	l.todos = items
	// The list is loop state first and a record second: a store that cannot
	// take this event will fail the next tool event, which does stop the run.
	_, _ = l.Recorder.Record(EvTodoUpdated, ActorAgent, Trusted, TodoList{Items: items, Note: note})
}

type Usage struct {
	InputTokens  int
	OutputTokens int
	CachedTokens int
	Turns        int
	// Compactions is a capacity metric, not a curiosity: each one invalidates
	// the prefix cache and pays cold prefill again (docs P8).
	Compactions int
	// ColdPrefillTokens counts input tokens that missed the cache. This is the
	// quantity the 17x prefix-cache claim is about, measured rather than assumed.
	ColdPrefillTokens int
}

// CacheHitRate reports the fraction of input tokens served from the prefix
// cache. Per docs P8 this is both a capacity and a UX metric, so it is surfaced
// rather than buried.
func (u Usage) CacheHitRate() float64 {
	if u.InputTokens == 0 {
		return 0
	}
	return float64(u.CachedTokens) / float64(u.InputTokens)
}

// PrefillSavings estimates the multiple by which prefix caching reduced prefill
// work this session: total input tokens over the tokens actually prefilled cold.
// This is the measured analogue of the computed 17x in docs/architecture/04-sizing.md.
func (u Usage) PrefillSavings() float64 {
	if u.ColdPrefillTokens == 0 {
		return 0
	}
	return float64(u.InputTokens) / float64(u.ColdPrefillTokens)
}

func NewLoop(a model.Adapter, reg *tools.Registry, pol *policy.Engine,
	appr Approver, sess *tools.Session, rec *Recorder, cfg Config) *Loop {
	l := &Loop{
		Adapter: a, Tools: reg, Policy: pol, Approver: appr,
		Session: sess, Recorder: rec, Config: cfg,
	}
	if cfg.OffloadAt > 0 {
		l.Offloader = NewOffloader(cfg.OffloadAt)
	}
	// recall is bound to this session's record, so it goes on a copy: a server
	// shares one registry across sessions, and adding it there would hand
	// every session the tool that reads the last one's record.
	if reg != nil && rec != nil && rec.store != nil {
		l.Tools = reg.Clone()
		l.Tools.Add(Recall{Store: rec.store, SessionID: rec.sessionID})
	}
	return l
}

// Continue runs another exchange on the SAME conversation.
//
// Run and Continue differ only in that Run is the first call. The message
// history, the read-tracking that makes editing safe, and the accumulated
// usage all persist on the Loop, so a follow-up like "now add a test for it"
// resolves against everything that came before.
//
// The turn counter is NOT reset: MaxTurns bounds the whole conversation, not
// each exchange, so a long back-and-forth cannot quietly exceed the budget an
// operator set.
func (l *Loop) Continue(ctx context.Context, userPrompt string) (TerminalReason, error) {
	return l.Run(ctx, userPrompt)
}

// Run executes turns until termination and returns the reason.
//
// Calling it again on the same Loop continues the conversation rather than
// starting over; see Continue.
func (l *Loop) Run(ctx context.Context, userPrompt string) (TerminalReason, error) {
	return l.RunMessage(ctx, Message{Text: userPrompt})
}

// RunMessage is Run for a prompt that carries a client's id, which the
// recorded user.message echoes so the client can match it.
func (l *Loop) RunMessage(ctx context.Context, m Message) (TerminalReason, error) {
	// Messages left queued by a run that ended first keep their place ahead
	// of the new prompt.
	if err := l.deliverQueued(); err != nil {
		return TermError, err
	}
	if _, err := l.Recorder.Record(EvUserMessage, ActorUser, Trusted, Message{Text: m.Text, ClientID: m.ClientID}); err != nil {
		return TermError, err
	}
	l.messages = append(l.messages, model.Message{Role: model.RoleUser, Content: m.Text})
	return l.run(ctx)
}

// RunQueued continues the conversation with only the queued messages, for a
// message that arrived after the last run had already decided to end.
func (l *Loop) RunQueued(ctx context.Context) (TerminalReason, error) {
	if len(l.Queued()) == 0 {
		return TermCompleted, nil
	}
	return l.run(ctx)
}

func (l *Loop) run(ctx context.Context) (TerminalReason, error) {
	for {
		if ctx.Err() != nil {
			return l.finish(terminalForCancel(ctx)), nil //nolint:nilerr // an interrupt is a terminal reason, not a failure
		}
		if err := l.recordFailure(); err != nil {
			return TermError, err
		}
		if l.turns >= l.Config.MaxTurns {
			return l.finish(TermMaxTurns), nil
		}
		// At the turn boundary, not mid-turn: cutting a turn short would leave
		// a tool result the model never sees.
		if l.Budget.Exhausted() {
			return l.finish(TermMaxBudget), nil
		}
		// Steering is applied before the turn is counted, so a redirection
		// never costs the user a turn from the budget.
		if err := l.deliverQueued(); err != nil {
			return TermError, err
		}
		l.turns++
		if l.Monitor != nil {
			l.Monitor.BeginTurn()
		}

		// Offload first: it is free and lossless, and often leaves compaction
		// with nothing to do.
		l.offloadIfNeeded()
		if err := l.maybeCompact(ctx); err != nil {
			// Compaction failure is not fatal on its own; the turn may still
			// fit. If it does not, the model call will say so.
			l.record(EvCompactDone, ActorSystem, map[string]string{
				"error": err.Error(),
			})
		}

		reason, done, err := l.turn(ctx)
		if err != nil {
			l.finish(TermError)
			return TermError, err
		}
		if done {
			// A message queued during the final turn is answered now rather
			// than left waiting for a prompt that may never come.
			if reason == TermCompleted && len(l.Queued()) > 0 {
				continue
			}
			return l.finish(reason), nil
		}
	}
}

// maybeCompact summarizes history when it approaches the context limit.
//
// The system prompt and memory file are re-injected whole rather than
// summarized: compaction discarding the operating rules is exactly the failure
// docs P4 warns about.
func (l *Loop) maybeCompact(ctx context.Context) error { return l.compactIfNeeded(ctx, true) }

// compactNow checks without a forward allowance, for the point after tool
// results have already been added to history.
func (l *Loop) compactNow(ctx context.Context) error { return l.compactIfNeeded(ctx, false) }

func (l *Loop) compactIfNeeded(ctx context.Context, reserve bool) error {
	if l.Compactor == nil {
		return nil
	}
	check := l.Compactor.ShouldCompact
	if !reserve {
		check = l.Compactor.ShouldCompactNow
	}
	should, used, err := check(l.Config.SystemPrompt, l.messages, toolDefs(l.Tools))
	if err != nil || !should {
		return err
	}

	l.record(EvCompactStarted, ActorSystem, Compaction{
		BeforeTokens: used, Trigger: "auto",
	})

	compacted, info, err := l.Compactor.Compact(ctx, "auto", l.Config.SystemPrompt, l.messages, used)
	if err != nil {
		return err
	}
	if len(compacted) == len(l.messages) {
		return nil // nothing was summarized
	}

	l.messages = compacted
	l.usage.Compactions++
	l.record(EvCompactDone, ActorSystem, info)
	return nil
}

// Compact forces compaction now, for the /compact command.
func (l *Loop) Compact(ctx context.Context) (Compaction, error) {
	if l.Compactor == nil {
		return Compaction{}, fmt.Errorf("compaction is not configured")
	}
	used, _ := l.Adapter.CountTokens(model.Request{
		System: l.Config.SystemPrompt, Messages: l.messages,
	})
	compacted, info, err := l.Compactor.Compact(ctx, "manual", l.Config.SystemPrompt, l.messages, used)
	if err != nil {
		return Compaction{}, err
	}
	l.messages = compacted
	l.usage.Compactions++
	l.record(EvCompactDone, ActorSystem, info)
	return info, nil
}

// SetAdapter swaps the model mid-session.
//
// The conversation is kept: the messages are provider-neutral, so a session can
// start on a fast local model and move to a larger one when the work turns out
// to be harder than it looked, without losing what has been established.
//
// This does invalidate the prefix cache — the new provider has never seen this
// prefix — so the next turn pays cold prefill. That is a real cost and the
// reason this was once a restart-only operation; it is a worse trade than
// making the user rebuild the session by hand, which pays the same cost and
// loses the history too.
func (l *Loop) SetAdapter(a model.Adapter) {
	l.Adapter = a
	if l.Compactor != nil {
		l.Compactor.Adapter = a
	}
}

// Messages exposes the current history for inspection and testing.
func (l *Loop) Messages() []model.Message { return l.messages }

// SetHistory seeds a fresh loop with a conversation reconstructed from the
// record (see Fork), so a session can be continued by a process that never
// ran it. turns is how many the earlier process used; the budget is for the
// whole conversation, and a continuation does not get a fresh one.
func (l *Loop) SetHistory(msgs []model.Message, turns int) {
	l.messages = append([]model.Message(nil), msgs...)
	if turns > l.turns {
		l.turns = turns
	}
}

// turn runs one round trip: model output plus any tool executions.
func (l *Loop) turn(ctx context.Context) (TerminalReason, bool, error) {
	req := model.Request{
		System:      l.Config.SystemPrompt,
		Messages:    l.messages,
		Tools:       toolDefs(l.Tools),
		MaxTokens:   l.Config.MaxTokens,
		Temperature: l.Config.Temperature,
		Effort:      l.effort(),
	}

	callStart := time.Now()
	stream, err := l.Adapter.Complete(ctx, req)
	if err != nil {
		l.record(EvModelCall, ActorSystem, ModelCall{
			Turn: l.turns, LatencyMS: time.Since(callStart).Milliseconds(), Error: err.Error(),
		})
		return TermError, true, fmt.Errorf("model call failed: %w", err)
	}
	var firstToken time.Duration
	var callUsage model.Usage

	var text strings.Builder
	var reasoning strings.Builder
	var pending strings.Builder // un-flushed delta fragment
	deltas := l.fragments()
	deltaN := 0
	lastFlush := time.Now()
	var calls []model.ToolCall
	var streamErr error
	var thinking strings.Builder // un-flushed reasoning fragment
	thoughts := l.fragments()
	thinkN := 0
	lastThink := time.Now()
	// The tail a secret could start in stays held until the stream ends, since
	// reasoning may resume after the reply has begun.
	flushThinking := func(final bool) {
		out := thoughts.push(thinking.String())
		if final {
			out += thoughts.flush()
		}
		thinking.Reset()
		lastThink = time.Now()
		if out != "" {
			thinkN++
			l.record(EvAgentReasoningDelta, ActorAgent, Delta{Text: out, Seq: thinkN})
		}
	}

	for chunk := range stream {
		if firstToken == 0 && chunk.Type != model.ChunkDone {
			firstToken = time.Since(callStart)
		}
		switch chunk.Type {
		case model.ChunkText:
			if thinking.Len() > 0 {
				flushThinking(false)
			}
			text.WriteString(chunk.Text)
			// Emit the fragment immediately. Waiting for the full reply makes a
			// 30-second answer feel like a hang; streaming makes the same wall
			// time feel responsive because the first token arrives in ~1s.
			//
			// Deltas are coalesced into whole words before emission: one event
			// per token would flood the stream and the store with no gain a
			// reader can perceive.
			pending.WriteString(chunk.Text)
			// Emit on a natural boundary OR after a short interval, whichever
			// comes first. Boundary alone is not enough: early tokens arrive
			// faster than boundaries occur, so the first visible fragment ended
			// up carrying seven tokens. A time floor also bounds the event rate
			// on a fast endpoint, which pure per-token emission would not.
			if flushable(pending.String()) || time.Since(lastFlush) > 40*time.Millisecond {
				if out := deltas.push(pending.String()); out != "" {
					deltaN++
					l.record(EvAgentDelta, ActorAgent, Delta{Text: out, Seq: deltaN})
				}
				pending.Reset()
				lastFlush = time.Now()
			}
		case model.ChunkReasoning:
			// Reasoning is recorded for display but never fed back as history:
			// it is not part of the conversation the model should condition on.
			// It streams in coarser parts than the reply, so a long think shows
			// progress without an event per token.
			reasoning.WriteString(chunk.Text)
			thinking.WriteString(chunk.Text)
			if thinking.Len() >= 160 || time.Since(lastThink) > 250*time.Millisecond {
				flushThinking(false)
			}
		case model.ChunkToolCall:
			calls = append(calls, *chunk.ToolCall)
		case model.ChunkError:
			streamErr = chunk.Err
		case model.ChunkDone:
			if chunk.Usage != nil {
				callUsage = *chunk.Usage
				l.usage.InputTokens += chunk.Usage.InputTokens
				l.usage.OutputTokens += chunk.Usage.OutputTokens
				l.usage.CachedTokens += chunk.Usage.CachedInputTokens
				l.usage.ColdPrefillTokens += chunk.Usage.InputTokens - chunk.Usage.CachedInputTokens
				l.Budget.Spend(chunk.Usage.InputTokens + chunk.Usage.OutputTokens)
			}
		}
	}

	flushThinking(true)
	// The stream has ended, cleanly or not, so nothing is held back any more.
	if out := deltas.push(pending.String()) + deltas.flush(); out != "" {
		deltaN++
		l.record(EvAgentDelta, ActorAgent, Delta{Text: out, Seq: deltaN})
	}
	pending.Reset()

	mc := ModelCall{
		Turn: l.turns, TokensIn: callUsage.InputTokens, TokensOut: callUsage.OutputTokens,
		TokensCached: callUsage.CachedInputTokens, CacheReported: callUsage.CacheReported,
		ContextWindow: l.Adapter.Profile().ContextWindow,
		FirstTokenMS:  firstToken.Milliseconds(), LatencyMS: time.Since(callStart).Milliseconds(),
		ToolCalls: len(calls),
		CutOff:    l.Config.MaxTokens > 0 && callUsage.OutputTokens >= l.Config.MaxTokens,
	}
	// A stream cut by our own cancel is an interrupt, recorded as such at
	// session end, not a model failure.
	if streamErr != nil && ctx.Err() == nil {
		mc.Error = streamErr.Error()
	}
	l.record(EvModelCall, ActorSystem, mc)

	if think := strings.TrimSpace(reasoning.String()); think != "" {
		l.noteReasoning(think)
		l.record(EvAgentReasoning, ActorAgent,
			Reasoning{Text: think, Turn: l.turns})
	}

	if ctx.Err() != nil {
		return terminalForCancel(ctx), true, nil //nolint:nilerr // an interrupt is a terminal reason, not a failure
	}

	// A malformed tool call is recoverable: tell the model what was wrong and
	// let it retry rather than aborting the session.
	if streamErr != nil && len(calls) == 0 {
		if text.Len() == 0 {
			// No empty assistant turn: an endpoint that rejects null content
			// would turn this recovery into the failure it exists to avoid.
			l.messages = append(l.messages,
				model.Message{Role: model.RoleUser, Content: "Your previous response could not be parsed: " +
					streamErr.Error() + "\nPlease retry with valid tool arguments."})
			return "", false, nil
		}
	}

	if body := text.String(); body != "" {
		if _, err := l.Recorder.Record(EvAgentMessage, ActorAgent, Trusted, Message{Text: body}); err != nil {
			return TermError, true, err
		}
	}

	// Normal termination: a response with no tool calls.
	if len(calls) == 0 {
		// An empty turn is not an answer. A reasoning model can spend its
		// whole turn thinking — ending on "Let's execute." — and emit neither
		// text nor a call; treating that as completion ends the session with
		// nothing said, which reads as the agent silently ignoring the
		// question. Prompt it once to continue, and only give up if it stalls
		// again, so a genuine end-of-turn still terminates immediately.
		// A turn cut off at the output limit is the same stall with more
		// tokens: the model was still reasoning when the budget ended it.
		// Sessions ended this way; some read as "completed" because a sentence
		// was there.
		if strings.TrimSpace(text.String()) == "" || mc.CutOff {
			l.emptyTurns++
			if mc.CutOff {
				l.dropEffort = true
			}
			if l.emptyTurns <= emptyTurnRetries {
				// Only the user turn is appended. An assistant message with
				// empty content is rejected outright by some endpoints
				// ("invalid message content type: <nil>"), which would turn a
				// recoverable stall into a failed session.
				nudge := "You produced no answer and called no tool. Continue: either call " +
					"the tool you intended, or write the answer itself."
				if mc.CutOff {
					nudge = "Your reply was cut off at the output limit before you acted. " +
						"Keep the thinking short and reply with the tool call you intended, " +
						"or with the answer itself in a few sentences."
				}
				l.messages = append(l.messages, model.Message{Role: model.RoleUser, Content: nudge})
				return "", false, nil
			}
			return TermStalled, true, nil
		}
		l.emptyTurns = 0
		l.messages = append(l.messages, model.Message{
			Role: model.RoleAssistant, Content: text.String(),
		})
		return TermCompleted, true, nil
	}
	l.emptyTurns = 0

	l.messages = append(l.messages, model.Message{
		Role: model.RoleAssistant, Content: text.String(), ToolCalls: calls,
	})

	if terminal := l.runCalls(ctx, calls); terminal != "" {
		return terminal, true, nil
	}

	// Compact here as well as before the turn. A single tool result can add
	// more than the whole compaction headroom — a 40,000-character file is
	// ~13,000 tokens — so a check that only runs before the turn watches
	// history step from comfortably under the threshold to over the hard limit
	// in one move, and the next model call fails with the window exceeded
	// rather than being compacted. Checking after the results land is what
	// makes a long session survive its own tool output.
	if err := l.compactNow(ctx); err != nil {
		l.record(EvCompactDone, ActorSystem, map[string]string{
			"error": err.Error(),
		})
	}

	// If one call has failed identically far past the point of escalation, the
	// model is stuck. Ending is better than spending the remaining budget.
	//
	// The parallel calls have all finished by here, but the lock is taken
	// anyway: relying on that ordering means a future edit that moves this
	// read, or starts a call that outlives the turn, crashes the process
	// rather than failing a test.
	if key, n, stuck := l.worstRepeatedFailure(); stuck {
		l.messages = append(l.messages, model.Message{
			Role: model.RoleUser,
			Content: fmt.Sprintf(
				"Stopping: the same call failed %d times without adaptation (%s).",
				n, truncateKey(key)),
		})
		return TermRetryExhausted, true, nil
	}
	return "", false, nil
}

const (
	// After this many identical failures the error message is escalated.
	repeatedFailureLimit = 3
	// After this many, the loop gives up rather than burning the budget.
	repeatedFailureAbort = 6
	// How many empty turns to nudge through before calling the run stalled.
	emptyTurnRetries = 2
)

func truncateKey(k string) string {
	if len(k) > 80 {
		return k[:80] + "…"
	}
	return k
}

// authorize puts one call through policy and, where needed, the approver.
//
// It is separate from running the tool so that a turn's approvals happen one at
// a time while the approved calls can then run together: two permission prompts
// racing for the same terminal is unusable, and the user cannot tell which one
// they are answering.
func (l *Loop) authorize(ctx context.Context, call model.ToolCall) (bool, tools.Result, TerminalReason) {
	tool, found := l.Tools.Get(call.Name)
	if !found {
		return false, tools.Result{
			Content: fmt.Sprintf("Unknown tool %q. Available tools: %s.",
				call.Name, strings.Join(l.Tools.Names(), ", ")),
			IsError: true,
		}, ""
	}

	decision := l.Policy.Evaluate(call.Name, tool.Mutates(), call.Args)
	// A command that asks for secrets is judged on each name first: a secret
	// needs an allow rule of its own, in every mode, or the call is refused.
	if refused := l.secretsRefused(call); refused != "" {
		decision = policy.Result{Decision: policy.Deny, Reason: refused, Step: "deny"}
	}

	// Only an Ask is short-circuited: a deny is still recorded as a deny.
	var doomed error
	if pc, ok := tool.(tools.Prechecker); ok && decision.Decision == policy.Ask {
		doomed = pc.Precheck(l.Session, call.Args)
	}
	if doomed != nil {
		decision.Reason = "refused before approval: the call could not succeed"
	}
	if l.Monitor != nil && doomed == nil {
		decision = l.reviewed(ctx, call, tool.Mutates(), decision)
	}

	asked, err := l.Recorder.Record(EvActionRequested, ActorAgent, Trusted, ActionRequested{
		CallID:           call.ID,
		Tool:             call.Name,
		Args:             call.Args,
		RequiresApproval: decision.Decision == policy.Ask && doomed == nil,
		Reason:           decision.Reason,
		Scope:            decision.Scope,
	})
	if err != nil {
		return false, tools.Result{Content: err.Error(), IsError: true}, TermError
	}
	if doomed != nil {
		return false, tools.Result{Content: doomed.Error(), IsError: true}, ""
	}

	switch decision.Decision {
	case policy.Deny:
		l.record(EvActionDenied, ActorSystem, map[string]string{
			"call_id": call.ID, "reason": decision.Reason, "step": decision.Step,
		})
		// Feed the denial back so the model can choose another approach.
		return false, tools.Result{
			Content: fmt.Sprintf("Denied: %s. Choose a different approach.", decision.Reason),
			IsError: true,
		}, ""

	case policy.Ask:
		approved, err := l.Approver.Approve(WithRequestID(ctx, asked.ID), call.Name, call.Args, decision)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return false, tools.Result{Content: "Interrupted.", IsError: true}, terminalForCancel(ctx)
			}
			return false, tools.Result{Content: err.Error(), IsError: true}, TermError
		}
		if !approved {
			l.record(EvActionDenied, ActorUser, map[string]string{
				"call_id": call.ID, "reason": "rejected: " + decision.Reason, "step": decision.Step,
			})
			// Say WHY, and name the rule that would have allowed it. A bare
			// "rejected" makes the model re-phrase the same command forever:
			// the first real run against a local model burned 20 turns doing
			// exactly that, because `cd x && go test` did not match bash(go *).
			msg := "This action was not approved"
			if decision.Reason != "" {
				msg += " (" + decision.Reason + ")"
			}
			msg += ".\n"
			if decision.Scope != "" {
				msg += "It would be permitted by the rule " + decision.Scope + ", which is not configured.\n"
			}
			msg += "Do not retry this call or a re-worded version of it. " +
				"Use a different tool, or explain what you need and stop."
			return false, tools.Result{Content: msg, IsError: true}, ""
		}
	}

	// by says who let it through: the policy on its own, or a person asked.
	by := "policy"
	if decision.Decision == policy.Ask {
		by = "reviewer"
	}
	l.record(EvActionApproved, ActorSystem, map[string]string{
		"call_id": call.ID, "reason": decision.Reason, "step": decision.Step, "by": by,
	})
	return true, tools.Result{}, ""
}

// invoke runs an already-authorized tool and records its observation.
func (l *Loop) invoke(ctx context.Context, call model.ToolCall) (tools.Result, TerminalReason) {
	tool, found := l.Tools.Get(call.Name)
	if !found {
		return tools.Result{Content: "tool disappeared between authorization and execution",
			IsError: true}, TermError
	}

	start := time.Now()
	result := tool.Run(ctx, l.Session, call.Args)
	// A secret's value is stripped from the result before the model and the
	// record see it, so the model never holds a value it could echo elsewhere.
	if red := l.Recorder.redactor(); red != nil {
		result.Content = redactedText(red.Redact, result.Content)
	}
	elapsed := time.Since(start)

	// An identical call that keeps failing means the model is not reading the
	// error. Escalate the message rather than letting it consume every turn:
	// the error text alone has demonstrably not worked.
	if result.IsError {
		l.failuresMu.Lock()
		if l.repeatedFailures == nil {
			l.repeatedFailures = map[string]int{}
		}
		key := call.Name + string(call.Args)
		l.repeatedFailures[key]++
		n := l.repeatedFailures[key]
		l.failuresMu.Unlock()
		if n >= repeatedFailureLimit {
			result.Content = fmt.Sprintf(
				"%s\n\n[This exact call has now failed %d times. Repeating it will not "+
					"work. Read the error above and do something different — or explain "+
					"what is blocking you and stop.]", result.Content, n)
		}
	} else {
		l.failuresMu.Lock()
		delete(l.repeatedFailures, call.Name+string(call.Args))
		l.failuresMu.Unlock()
	}

	// Tool output is untrusted: it may contain text that looks like
	// instructions. The trust tag travels with the event (docs arch §6).
	trust := Untrusted
	l.noteCall(call.Name, policy.Subject(call.Name, call.Args), result.IsError)
	if _, err := l.Recorder.Record(EvObservation, ActorTool, trust, Observation{
		CallID:     call.ID,
		Tool:       call.Name,
		Content:    result.Content,
		IsError:    result.IsError,
		Truncated:  result.Truncated,
		ExitCode:   result.ExitCode,
		DurationMS: elapsed.Milliseconds(),
	}); err != nil {
		return result, TermError
	}
	return result, ""
}

func (l *Loop) finish(reason TerminalReason) TerminalReason {
	l.usage.Turns = l.turns
	ctxTokens, window := l.contextSize()
	l.record(EvSessionEnded, ActorSystem, SessionEnded{
		Reason:        reason,
		Turns:         l.turns,
		TokensIn:      l.usage.InputTokens,
		TokensOut:     l.usage.OutputTokens,
		TokensCached:  l.usage.CachedTokens,
		Compactions:   l.usage.Compactions,
		ContextTokens: ctxTokens,
		ContextWindow: window,
	})
	return reason
}

// contextSize measures the conversation as it now stands, which is what the
// next turn would send. It is deliberately separate from l.usage.InputTokens:
// that is a cumulative cost counter, this is an occupancy reading.
//
// Errors are swallowed to zero rather than returned. This runs while a session
// is ending, often because something already went wrong, and a token estimate
// that cannot be produced is not a reason to fail the termination that reports
// the real problem. Zero reads as "not measured" at every consumer.
func (l *Loop) contextSize() (used, window int) {
	if l.Adapter == nil {
		return 0, 0
	}
	window = l.Adapter.Profile().ContextWindow
	n, err := l.Adapter.CountTokens(model.Request{
		System:   l.Config.SystemPrompt,
		Messages: l.messages,
		Tools:    toolDefs(l.Tools),
	})
	if err != nil {
		return 0, window
	}
	return n, window
}

func (l *Loop) Usage() Usage { return l.usage }

// flushable reports whether a buffered fragment should be emitted now.
//
// The first version coalesced only on trailing whitespace and punctuation,
// which collapsed a 21-token reply into 2 events: a fragment like "\n2" ends
// on a digit, so it buffered until it hit the length cap. Streaming that the
// user cannot see is not streaming.
//
// Leading whitespace counts too — "\n2" is a word boundary at its START — and
// the length cap is small enough that even unbroken text emits several times a
// second. The aim is text that visibly grows, not one event per token.
func flushable(s string) bool {
	if s == "" {
		return false
	}
	if len(s) >= 12 {
		return true
	}
	// A fragment that BEGINS a new word or line can be emitted immediately:
	// whatever preceded it is already complete.
	switch s[0] {
	case ' ', '\n', '\t':
		return true
	}
	switch s[len(s)-1] {
	case ' ', '\n', '\t', '.', ',', ':', ';', '!', '?', ')', ']', '}':
		return true
	}
	return false
}

func toolDefs(r *tools.Registry) []model.ToolDef {
	defs := r.Definitions()
	out := make([]model.ToolDef, len(defs))
	for i, d := range defs {
		out[i] = model.ToolDef{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema}
	}
	return out
}

// fitResult bounds what one tool result can occupy in history.
//
// Compaction summarizes what is already there; it cannot help with a single
// message too large to send. A tool that returns a whole file, or a retrieval
// that returns twenty documents, can exceed the model's window on its own, and
// no amount of summarizing earlier turns rescues a request whose last message
// does not fit.
//
// The middle is dropped rather than the tail: the head of a result says what it
// is and the tail usually carries the conclusion, while the middle of a long
// listing is the least load-bearing part. The elision is explicit so the model
// knows it is reading an excerpt and can ask for the rest by offset.
func (l *Loop) fitResult(content string) string {
	limit := l.resultLimitChars()
	if limit <= 0 || len(content) <= limit {
		return content
	}
	half := limit / 2
	dropped := len(content) - limit
	return content[:half] +
		fmt.Sprintf("\n\n[... %d characters elided to fit the context window. "+
			"Re-read with an offset to see this part. ...]\n\n", dropped) +
		content[len(content)-half:]
}

// resultLimitChars is the per-result cap, derived from the context window so a
// small-window model is protected and a large-window one is not needlessly
// truncated. Zero means the window is unknown, and nothing is capped.
func (l *Loop) resultLimitChars() int {
	window := l.Adapter.Profile().ContextWindow
	if window <= 0 {
		return 0
	}
	// A quarter of the window, converted back to characters at the same
	// ~3.6 chars/token the estimator uses. One result may occupy a quarter of
	// the budget; four such results in one turn still leave room to compact.
	return window / 4 * 36 / 10
}

// LoopHolder lets a tool built before the loop report into it once it exists.
//
// The registry is constructed first — tools have to be known before a loop can
// be given them — so a tool that needs to record an event has nothing to record
// into yet. A holder makes that ordering explicit and scoped, where a package
// variable would silently share one loop across every session in the process.
type LoopHolder struct{ loop *Loop }

func (h *LoopHolder) Set(l *Loop) { h.loop = l }

// RecordTodos forwards to the current loop, and does nothing before one is set.
func (h *LoopHolder) RecordTodos(items []Todo, note string) {
	if h != nil && h.loop != nil {
		h.loop.RecordTodos(items, note)
	}
}

// RecordPipelineStage forwards to the current loop.
func (h *LoopHolder) RecordPipelineStage(skill, stage, detail string, data map[string]any) {
	if h != nil && h.loop != nil {
		h.loop.RecordPipelineStage(skill, stage, detail, data)
	}
}

// runCalls executes a turn's tool calls and appends their results.
//
// Independent calls run concurrently. A model that asks to read four files
// should not wait for four round trips in series, and the prompt asking it to
// batch calls was only ever a request — the harness either runs them together
// or it does not.
//
// Three things are deliberately NOT parallel:
//
// Approval is sequential, because two permission prompts racing for one
// terminal is unusable and the user cannot tell which they are answering.
// Every call is put through policy first, in order, and only the approved ones
// are then run together.
//
// Mutating calls are sequential with respect to everything. Two edits to the
// same file, or an edit racing a read of it, produce a result that depends on
// scheduling — and a session that cannot be replayed to the same outcome is not
// auditable, which is the property the whole event log exists to provide.
//
// Results are appended in the order the model asked for them, never in the
// order they finished, for the same reason.
func (l *Loop) runCalls(ctx context.Context, calls []model.ToolCall) TerminalReason {
	results := make([]callOutcome, len(calls))

	// Phase 1: policy and approval, in order, one at a time.
	approved := make([]bool, len(calls))
	for i, call := range calls {
		decision, res, terminal := l.authorize(ctx, call)
		if terminal != "" {
			results[i] = callOutcome{result: res, terminal: terminal}
			l.appendResults(calls, results, i+1)
			return terminal
		}
		if !decision {
			results[i] = callOutcome{result: res}
			continue
		}
		approved[i] = true
	}

	// Phase 2: run what was approved. Read-only calls go together; anything
	// that mutates runs alone, after the concurrent batch, so the outcome does
	// not depend on which goroutine won.
	var wg sync.WaitGroup
	var mutating []int
	for i, call := range calls {
		if !approved[i] {
			continue
		}
		tool, found := l.Tools.Get(call.Name)
		if found && tool.Mutates() {
			mutating = append(mutating, i)
			continue
		}
		wg.Add(1)
		go func(i int, call model.ToolCall) {
			defer wg.Done()
			res, terminal := l.invoke(ctx, call)
			results[i] = callOutcome{result: res, terminal: terminal}
		}(i, call)
	}
	wg.Wait()

	for _, i := range mutating {
		res, terminal := l.invoke(ctx, calls[i])
		results[i] = callOutcome{result: res, terminal: terminal}
		if terminal != "" {
			l.appendResults(calls, results, i+1)
			return terminal
		}
	}

	l.appendResults(calls, results, len(calls))
	for _, o := range results {
		if o.terminal != "" {
			return o.terminal
		}
	}
	// A tool that IS the answer ends the run. The result is already recorded
	// and appended, so the transcript is complete; what is skipped is the
	// model's next turn, which would only restate it.
	for _, o := range results {
		if o.result.Final {
			return TermCompleted
		}
	}
	return ""
}

// worstRepeatedFailure reports a call that has failed past the abort threshold.
func (l *Loop) worstRepeatedFailure() (string, int, bool) {
	l.failuresMu.Lock()
	defer l.failuresMu.Unlock()
	for key, n := range l.repeatedFailures {
		if n >= repeatedFailureAbort {
			return key, n, true
		}
	}
	return "", 0, false
}

// callOutcome pairs a tool result with any terminal reason it produced.
type callOutcome struct {
	result   tools.Result
	terminal TerminalReason
}

// appendResults adds the first n results to history, in call order.
func (l *Loop) appendResults(calls []model.ToolCall, results []callOutcome, n int) {
	for i := 0; i < n && i < len(calls); i++ {
		l.messages = append(l.messages, model.Message{
			Role:       model.RoleTool,
			ToolCallID: calls[i].ID,
			Content:    l.fitResult(results[i].result.Content),
			IsError:    results[i].result.IsError,
		})
	}
}

// RecordPipelineStage records what a skill's pipeline did at one stage.
//
// A pipeline runs inside a single tool call, so without this a reader sees one
// opaque "skill" call and no sign of the decomposition, the sufficiency verdict
// or why a second retrieval happened. Recording each stage keeps the property
// the event log exists for: what the agent did is visible, not inferred.
func (l *Loop) RecordPipelineStage(skill, stage, detail string, data map[string]any) {
	payload := map[string]any{"skill": skill, "stage": stage, "detail": detail}
	for k, v := range data {
		payload[k] = v
	}
	l.record(EvPlanUpdated, ActorSystem, payload)
}

// record writes one of the loop's own events, which are all trusted, and
// keeps the first failure; see recordErr.
func (l *Loop) record(t EventType, actor Actor, payload any) {
	if _, err := l.Recorder.Record(t, actor, Trusted, payload); err != nil {
		l.recordMu.Lock()
		if l.recordErr == nil {
			l.recordErr = err
		}
		l.recordMu.Unlock()
	}
}

// recordFailure reports the first event that could not be written, if any.
func (l *Loop) recordFailure() error {
	l.recordMu.Lock()
	defer l.recordMu.Unlock()
	return l.recordErr
}

// secretsRefused names the first secret in the call that policy does not
// allow outright, or "" when the call asks for none or every one is allowed.
func (l *Loop) secretsRefused(call model.ToolCall) string {
	var a struct {
		Secrets []string `json:"secrets"`
	}
	if json.Unmarshal(call.Args, &a) != nil || len(a.Secrets) == 0 {
		return ""
	}
	// Rules only, never the mode: bypass and auto approve calls, not secrets.
	for _, name := range a.Secrets {
		for _, r := range l.Policy.Deny {
			if r.Matches("secret", name) {
				return fmt.Sprintf("secret %s is denied by rule %s", name, r)
			}
		}
		allowed := false
		for _, r := range l.Policy.Allow {
			allowed = allowed || r.Matches("secret", name)
		}
		if !allowed {
			return fmt.Sprintf("secret %s is not permitted: a secret needs its own allow rule, secret(%s), in every mode", name, name)
		}
	}
	return ""
}

// fragmentBuffer redacts a reply streamed in fragments. Redaction runs per
// event, so a secret split across two fragments would match in neither; the
// buffer holds back a tail that could still be the start of one.
type fragmentBuffer struct {
	redact func([]byte) []byte
	span   int
	carry  string // raw text not yet emitted
}

// fragments returns a buffer for one streamed reply. With no secrets it holds
// nothing back and passes fragments through unchanged.
func (l *Loop) fragments() *fragmentBuffer {
	b := &fragmentBuffer{}
	if red := l.Recorder.redactor(); red != nil {
		b.redact, b.span = red.Redact, red.Span()
	}
	return b
}

// push adds a fragment and returns the redacted text that can be emitted now.
// At least the last span-1 bytes are held back: too short to hold a whole
// secret, they may be the start of one.
func (b *fragmentBuffer) push(s string) string {
	if b.span == 0 {
		return s
	}
	raw := b.carry + s
	whole := redactedText(b.redact, raw)
	for cut := len(raw) - (b.span - 1); cut > 0; cut -= max(b.span-1, 1) {
		for cut > 0 && cut < len(raw) && !utf8.RuneStart(raw[cut]) {
			cut--
		}
		// A cut is safe only where splitting changes nothing. A secret that
		// crosses it starts less than a span before it, so the next try is there.
		head := redactedText(b.redact, raw[:cut])
		if cut > 0 && head+redactedText(b.redact, raw[cut:]) == whole {
			b.carry = raw[cut:]
			return head
		}
	}
	b.carry = raw
	return ""
}

// flush returns what was held back, redacted, once the stream has ended.
func (b *fragmentBuffer) flush() string {
	s := b.carry
	b.carry = ""
	if b.span == 0 || s == "" {
		return s
	}
	return redactedText(b.redact, s)
}

// redactedText runs a payload redactor over one string, through the JSON
// form the redactor matches against. It fails closed: text that cannot be
// redacted is withheld, never returned as it was.
func redactedText(redact func([]byte) []byte, text string) string {
	raw, err := json.Marshal(text)
	if err != nil {
		return Withheld
	}
	var out string
	if json.Unmarshal(redact(raw), &out) != nil {
		return Withheld
	}
	return out
}

// noteReasoning keeps the last few stated thoughts for the monitor.
func (l *Loop) noteReasoning(text string) {
	l.monitorMu.Lock()
	defer l.monitorMu.Unlock()
	l.reasoning = append(l.reasoning, text)
	if len(l.reasoning) > 6 {
		l.reasoning = l.reasoning[len(l.reasoning)-6:]
	}
}

// reviewed puts a call to the monitor and returns the decision after it,
// which is never looser than before. What the judge said is in the record.
func (l *Loop) reviewed(ctx context.Context, call model.ToolCall, mutates bool, before policy.Result) policy.Result {
	var remit, user strings.Builder
	for _, m := range l.messages {
		if m.Role == model.RoleUser {
			if remit.Len() == 0 {
				remit.WriteString(m.Content)
			}
			user.WriteString(m.Content)
			user.WriteByte('\n')
		}
	}
	args := string(call.Args)
	l.monitorMu.Lock()
	thoughts := append([]string(nil), l.reasoning...)
	recent := append([]monitor.Recent(nil), l.recentCalls...)
	l.monitorMu.Unlock()
	c := monitor.Case{
		Remit: remit.String(), Reasoning: thoughts,
		Tool: call.Name, Args: args, Mutates: mutates, Provisional: before,
		Provenance: monitor.Trace(args, user.String()), Recent: recent,
	}
	out := l.Monitor.Review(ctx, c)
	if out.Skipped != "" {
		return before
	}
	l.record(EvMonitorVerdict, ActorSystem, MonitorVerdict{
		CallID: call.ID, Before: string(out.Before), After: string(out.After), Code: out.Verdict.Code,
		Confidence: out.Verdict.Confidence, Rationale: out.Verdict.Rationale, LatencyMS: out.Verdict.LatencyMS,
		Version: out.Verdict.Version, Unavailable: out.Unavailable,
	})
	if !out.Tightened {
		return before
	}
	why := out.Verdict.Code
	if out.Verdict.Rationale != "" {
		why += " — " + out.Verdict.Rationale
	}
	return policy.Result{Decision: out.After, Reason: "monitor: " + why, Scope: before.Scope, Step: "monitor"}
}

// noteCall keeps the last few calls and outcomes for the monitor.
func (l *Loop) noteCall(tool, subject string, isError bool) {
	l.monitorMu.Lock()
	defer l.monitorMu.Unlock()
	l.recentCalls = append(l.recentCalls, monitor.Recent{Tool: tool, Subject: subject, IsError: isError})
	if len(l.recentCalls) > 5 {
		l.recentCalls = l.recentCalls[len(l.recentCalls)-5:]
	}
}

// effort is the reasoning effort for the next call: the configured one,
// lowered once a turn has spent its whole budget thinking without acting.
func (l *Loop) effort() model.EffortLevel {
	if l.dropEffort {
		return model.EffortLow
	}
	return l.Config.Effort
}
