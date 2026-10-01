package agent

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// WorkKind is what a WorkItem is: a subagent, or a background job of
// another kind such as a shell.
type WorkKind string

const (
	WorkAgent WorkKind = "agent"
	WorkShell WorkKind = "shell"
)

// WorkItem is one piece of a conversation's work besides the conversation
// itself, as a surface lists it: a subagent in the foreground or the
// background, or a background job.
type WorkItem struct {
	ID string `json:"id"`
	// Parent is the session that started it: the conversation's own id, or
	// a subagent's for a nested one.
	Parent     string   `json:"parent,omitempty"`
	Kind       WorkKind `json:"kind"`
	AgentType  string   `json:"agent_type,omitempty"`
	Title      string   `json:"title"`
	Background bool     `json:"background,omitempty"`
	// Status is running, completed, failed or cancelled.
	Status  string    `json:"status"`
	Reason  string    `json:"reason,omitempty"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitzero"`
	// Activity is what it is doing now: the last tool it called, or thinking.
	Activity  string `json:"activity,omitempty"`
	TokensIn  int    `json:"tokens_in,omitempty"`
	TokensOut int    `json:"tokens_out,omitempty"`
	// Undelivered are messages sent to it that it ended before taking.
	Undelivered []string `json:"undelivered,omitempty"`
}

// Running reports whether the item has not ended.
func (w WorkItem) Running() bool { return w.Status == "running" }

// Work is a conversation's live list of subagents, shared by every loop in
// its tree. A surface reads it; the loops write it as their children run.
type Work struct {
	mu    sync.Mutex
	items map[string]*workEntry
	order []string
}

type workEntry struct {
	item WorkItem
	loop *Loop
}

// NewWork makes an empty list.
func NewWork() *Work { return &Work{items: map[string]*workEntry{}} }

// Items lists every item, in the order they first started.
func (w *Work) Items() []WorkItem {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]WorkItem, 0, len(w.order))
	for _, id := range w.order {
		it := w.items[id].item
		it.Undelivered = append([]string(nil), it.Undelivered...)
		out = append(out, it)
	}
	return out
}

// Item reports one item.
func (w *Work) Item(id string) (WorkItem, bool) {
	if w == nil {
		return WorkItem{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.items[id]
	if !ok {
		return WorkItem{}, false
	}
	return e.item, true
}

// ErrNotRunning refuses a message to an item that has ended or takes none.
var ErrNotRunning = errors.New("it is not running, so it cannot take a message")

// Steer queues text for a running subagent, as the person's message: it is
// recorded in the subagent's own record when it takes it, at its next step.
func (w *Work) Steer(id, text string) error {
	if w == nil {
		return ErrNotRunning
	}
	w.mu.Lock()
	e, ok := w.items[id]
	if !ok || !e.item.Running() || e.loop == nil {
		w.mu.Unlock()
		return ErrNotRunning
	}
	l := e.loop
	w.mu.Unlock()
	l.Steer(text)
	return nil
}

// start adds or restarts a subagent as it begins a run.
func (w *Work) start(it WorkItem, l *Loop) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	e, again := w.items[it.ID]
	if !again {
		w.order = append(w.order, it.ID)
		e = &workEntry{}
		w.items[it.ID] = e
	} else {
		// A resumed run keeps its role, title and tokens so far.
		it.TokensIn, it.TokensOut = e.item.TokensIn, e.item.TokensOut
		if it.AgentType == "" {
			it.AgentType = e.item.AgentType
		}
		if it.Title == "" {
			it.Title = e.item.Title
		}
	}
	it.Status, it.Activity = "running", "starting"
	e.item, e.loop = it, l
}

// end marks a subagent's run over, with what was sent to it and not taken.
func (w *Work) end(id, status, reason string, undelivered []string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.items[id]
	if !ok {
		return
	}
	e.item.Status, e.item.Reason, e.item.Ended, e.item.Activity = status, reason, time.Now(), ""
	e.item.Undelivered = undelivered
}

// observe follows a subagent's recorded events: what it is doing, and the
// tokens its model calls took.
func (w *Work) observe(id string, ev Event) {
	if w == nil {
		return
	}
	activity, in, out := "", 0, 0
	switch ev.Type {
	case EvActionRequested:
		var a ActionRequested
		if json.Unmarshal(ev.Payload, &a) == nil {
			activity = a.Tool
			if s := policy.Subject(a.Tool, a.Args); s != "" {
				activity += " " + s
			}
		}
	case EvObservation, EvUserMessage, EvAgentReasoningDelta:
		activity = "thinking"
	case EvAgentDelta:
		activity = "writing"
	case EvModelCall:
		var mc ModelCall
		if json.Unmarshal(ev.Payload, &mc) == nil {
			in, out = mc.TokensIn, mc.TokensOut
		}
	default:
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.items[id]
	if !ok || !e.item.Running() {
		return
	}
	if activity != "" {
		e.item.Activity = activity
	}
	e.item.TokensIn += in
	e.item.TokensOut += out
}
