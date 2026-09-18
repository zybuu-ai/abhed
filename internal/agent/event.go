// Package agent implements Abhed's event-sourced agent loop.
//
// State is a chronological stream of actions and observations (docs P6). The
// agent is a function of event history to action; the runtime is a function of
// action to observation. Everything the loop does is recorded, which is what
// makes audit, replay, and evaluation possible without extra machinery.
package agent

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type EventType string

const (
	EvSessionStarted EventType = "session.started"
	EvUserMessage    EventType = "user.message"
	EvAgentMessage   EventType = "agent.message"
	// EvAgentDelta carries a fragment of the model's reply as it arrives.
	// The complete text still lands in EvAgentMessage, so a replayed session
	// reads identically whether or not the deltas were observed live.
	EvAgentDelta EventType = "agent.delta"
	// EvAgentReasoning carries the model's thinking, when the endpoint reports
	// it separately from the reply. It is displayed but never fed back as
	// history: it is not part of the conversation the model conditions on.
	// Recording it is what lets a user see WHY an answer came out as it did.
	EvAgentReasoning  EventType = "agent.reasoning"
	EvActionRequested EventType = "action.requested"
	EvActionApproved  EventType = "action.approved"
	EvActionDenied    EventType = "action.denied"
	EvObservation     EventType = "observation"
	EvSubagentSpawned EventType = "subagent.spawned"
	EvSubagentReturn  EventType = "subagent.returned"
	EvCompactStarted  EventType = "compaction.started"
	EvCompactDone     EventType = "compaction.completed"
	EvPlanUpdated     EventType = "plan.updated"
	EvTodoUpdated     EventType = "todo.updated"
	EvSessionEnded    EventType = "session.ended"
)

type Actor string

const (
	ActorUser   Actor = "user"
	ActorAgent  Actor = "agent"
	ActorSystem Actor = "system"
	ActorTool   Actor = "tool"
)

// Trust marks provenance. Content read from files, tool output, MCP responses
// and search results is untrusted: it is data, never instruction (docs arch §6).
// The tag is set at ingest and travels with the event.
type Trust string

const (
	Trusted   Trust = "trusted"
	Untrusted Trust = "untrusted"
)

// TerminalReason enumerates every way a session can end. These are distinct
// because CI exit codes and the eval harness must tell them apart: "the task
// failed" and "the budget ran out" call for different responses (docs §10).
type TerminalReason string

const (
	TermCompleted TerminalReason = "completed"
	TermMaxTurns  TerminalReason = "max_turns"
	TermMaxBudget TerminalReason = "max_budget"
	// Reserved, not emitted: a denial is fed back to the model as a recoverable
	// error so it can choose another approach, rather than ending the session.
	TermPolicyDenied   TerminalReason = "policy_denied"
	TermUserInterrupt  TerminalReason = "user_interrupt"
	TermError          TerminalReason = "error"
	TermShutdown       TerminalReason = "shutdown"
	TermRetryExhausted TerminalReason = "retry_exhausted"
	// TermStalled: the model produced neither text nor a tool call, repeatedly.
	// Distinct from completed because nothing was answered, and distinct from
	// error because nothing failed.
	TermStalled TerminalReason = "stalled"
)

// ExitCode maps a terminal reason to a process exit code for headless runs.
func (r TerminalReason) ExitCode() int {
	switch r {
	case TermCompleted:
		return 0
	case TermMaxTurns:
		return 2
	case TermMaxBudget:
		return 3
	case TermPolicyDenied:
		return 4
	case TermUserInterrupt:
		return 130
	case TermRetryExhausted:
		return 5
	case TermShutdown:
		return 6
	default:
		return 1
	}
}

type Event struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	ParentID  string          `json:"parent_id,omitempty"`
	Seq       int64           `json:"seq"`
	Type      EventType       `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	Actor     Actor           `json:"actor"`
	Trust     Trust           `json:"trust"`
	CreatedAt time.Time       `json:"created_at"`
}

// Payload shapes.

type ActionRequested struct {
	CallID           string          `json:"call_id"`
	Tool             string          `json:"tool"`
	Args             json.RawMessage `json:"args"`
	RequiresApproval bool            `json:"requires_approval"`
	Reason           string          `json:"reason,omitempty"`
}

type Observation struct {
	CallID     string `json:"call_id"`
	Tool       string `json:"tool"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error"`
	Truncated  bool   `json:"truncated"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

type Message struct {
	Text string `json:"text"`
}

// Delta is one streamed fragment of an agent message.
type Delta struct {
	Text string `json:"text"`
	Seq  int    `json:"n"` // ordinal within this message, for ordering
}

// Reasoning is the model's thinking for one turn, recorded whole rather than
// streamed: it is reference material a reader opens after the fact, not
// something they follow token by token, and one event per turn keeps it out of
// the way of the reply that matters.
type Reasoning struct {
	Text string `json:"text"`
	Turn int    `json:"turn"`
}

type SessionEnded struct {
	Reason       TerminalReason `json:"reason"`
	Turns        int            `json:"turns"`
	TokensIn     int            `json:"tokens_in"`
	TokensOut    int            `json:"tokens_out"`
	TokensCached int            `json:"tokens_cached"`
	Compactions  int            `json:"compactions"`

	// ContextTokens is what the NEXT turn would send: the system prompt, the
	// conversation as it now stands, and the tool definitions. TokensIn above
	// is a different quantity — the running sum of every turn's prompt, which
	// only ever grows and says nothing about how full the window is.
	//
	// Both are worth reporting and they were being conflated. A ten-turn
	// session reading one 9,800-token document reached 138,048 TokensIn while
	// never exceeding 13,982 in context, and the cumulative figure read as a
	// session about to overflow a 32,768 window that was in fact half empty.
	ContextTokens int `json:"context_tokens,omitempty"`
	// ContextWindow is the model's limit, so a reader can see the ratio
	// without knowing which model answered. Zero when the adapter does not
	// report one, which is also when compaction never fires.
	ContextWindow int `json:"context_window,omitempty"`
}

// Todo is one item in the agent's task list.
type Todo struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"` // pending | in_progress | done | cancelled
}

// TodoList is the whole list, recorded whole on every change.
//
// Recording the list rather than a diff is deliberate: a reader replaying the
// session sees what the agent believed the plan was at each point, without
// having to reconstruct it from increments. The list is small and the clarity
// is worth the repetition.
type TodoList struct {
	Items []Todo `json:"items"`
	// Note explains the change, when the agent gives a reason for it.
	Note string `json:"note,omitempty"`
}

type Compaction struct {
	BeforeTokens int    `json:"before_tokens"`
	AfterTokens  int    `json:"after_tokens"`
	Summary      string `json:"summary,omitempty"`
	Trigger      string `json:"trigger"` // auto | manual
}

// Store persists events. Append-only by contract: no update, no delete.
// Retention is handled by dropping partitions, so the audit guarantee holds.
type Store interface {
	Append(ev Event) error
	Events(sessionID string) ([]Event, error)
	// Since supports SSE resumption via Last-Event-ID (docs §10).
	Since(sessionID string, seq int64) ([]Event, error)
}

// SessionDeleter is implemented by stores that can forget a session.
//
// Optional rather than part of Store, because "delete" is not meaningful for
// every backend — an append-only audit log in a regulated deployment must NOT
// support it, and a store that silently ignored the call would be worse than
// one that never offered it. The server checks for this interface and reports
// honestly when it is absent.
type SessionDeleter interface {
	DeleteSession(sessionID string) error
}

// MemStore is an in-memory Store for local development and tests. The
// production path uses Postgres with the schema in docs/architecture/10-data-model.md.
type MemStore struct {
	mu     sync.RWMutex
	events map[string][]Event
	subs   map[string][]chan Event
}

func NewMemStore() *MemStore {
	return &MemStore{
		events: make(map[string][]Event),
		subs:   make(map[string][]chan Event),
	}
}

// DeleteSession forgets a session's events. Subscribers are left alone: a live
// stream that is cut mid-run should end because the run ended, not because the
// rows vanished underneath it.
func (m *MemStore) DeleteSession(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.events, sessionID)
	return nil
}

func (m *MemStore) Append(ev Event) error {
	m.mu.Lock()
	m.events[ev.SessionID] = append(m.events[ev.SessionID], ev)
	subs := append([]chan Event(nil), m.subs[ev.SessionID]...)
	m.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // never block the loop on a slow consumer
		}
	}
	return nil
}

func (m *MemStore) Events(sessionID string) ([]Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Event, len(m.events[sessionID]))
	copy(out, m.events[sessionID])
	return out, nil
}

func (m *MemStore) Since(sessionID string, seq int64) ([]Event, error) {
	all, err := m.Events(sessionID)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, e := range all {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	return out, nil
}

// Subscribe streams events for a session. Used by the CLI renderer and,
// in server mode, by the SSE endpoint.
func (m *MemStore) Subscribe(sessionID string) <-chan Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan Event, 256)
	m.subs[sessionID] = append(m.subs[sessionID], ch)
	return ch
}

func (m *MemStore) Unsubscribe(sessionID string, ch <-chan Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	subs := m.subs[sessionID]
	for i, c := range subs {
		if c == ch {
			m.subs[sessionID] = append(subs[:i], subs[i+1:]...)
			close(c)
			return
		}
	}
}

// Recorder assigns sequence numbers and timestamps, so callers never have to.
type Recorder struct {
	store     Store
	sessionID string
	parentID  string
	mu        sync.Mutex
	seq       int64
}

func NewRecorder(store Store, sessionID, parentID string) *Recorder {
	return &Recorder{store: store, sessionID: sessionID, parentID: parentID}
}

// Advance moves the sequence past events already in the store, so a session
// continued from its record keeps one monotonic sequence rather than
// colliding with the rows it is continuing from.
func (r *Recorder) Advance(seq int64) {
	r.mu.Lock()
	if seq > r.seq {
		r.seq = seq
	}
	r.mu.Unlock()
}

func (r *Recorder) Record(t EventType, actor Actor, trust Trust, payload any) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("marshal %s payload: %w", t, err)
	}

	r.mu.Lock()
	r.seq++
	ev := Event{
		ID:        newID(),
		SessionID: r.sessionID,
		ParentID:  r.parentID,
		Seq:       r.seq,
		Type:      t,
		Payload:   raw,
		Actor:     actor,
		Trust:     trust,
		CreatedAt: time.Now().UTC(),
	}
	r.mu.Unlock()

	return ev, r.store.Append(ev)
}
