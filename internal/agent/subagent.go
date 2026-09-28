package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Budget enforces hierarchical spend limits across a session and its subagents.
//
// Subagent spend counts against the parent's cap (docs §02 §3). Without this a
// fan-out multiplies cost invisibly: each subagent re-prefills its own system
// prompt and memory file, so 20 subagents is 20 cold prefills, not one.
type Budget struct {
	MaxTokens    int64
	MaxSubagents int
	AllowNested  bool

	tokens  atomic.Int64
	spawned atomic.Int32
}

func NewBudget(maxTokens int64, maxSubagents int, allowNested bool) *Budget {
	return &Budget{MaxTokens: maxTokens, MaxSubagents: maxSubagents, AllowNested: allowNested}
}

func (b *Budget) Spend(tokens int) {
	if b == nil {
		return
	}
	b.tokens.Add(int64(tokens))
}

func (b *Budget) Exhausted() bool {
	if b == nil || b.MaxTokens <= 0 {
		return false
	}
	return b.tokens.Load() >= b.MaxTokens
}

func (b *Budget) Spent() int64 {
	if b == nil {
		return 0
	}
	return b.tokens.Load()
}

// TryReserveSubagent accounts for one spawn, or explains the refusal.
func (b *Budget) TryReserveSubagent() error {
	if b == nil {
		return nil
	}
	if b.Exhausted() {
		return fmt.Errorf("budget limit reached (%d tokens spent of %d)", b.tokens.Load(), b.MaxTokens)
	}
	if b.MaxSubagents > 0 && int(b.spawned.Load()) >= b.MaxSubagents {
		return fmt.Errorf("subagent limit reached (%d of %d spawned)", b.spawned.Load(), b.MaxSubagents)
	}
	b.spawned.Add(1)
	return nil
}

// Task spawns a subagent with a fresh context.
//
// The subagent explores using however many tokens it needs and returns a
// bounded summary, so the orchestrator's context grows by the summary rather
// than the full transcript (docs P3). Isolation is a compression ratio, not
// free: each spawn re-prefills its own prefix.
type Task struct {
	// Spawn runs a subagent and returns its summary. Injected so the tool does
	// not have to know how a Loop is constructed.
	Spawn func(ctx context.Context, req SubagentRequest) (string, error)
	// Profiles limits which agent types may be requested.
	Profiles map[string]PromptProfile
}

type SubagentRequest struct {
	Prompt      string
	Description string
	AgentType   string
	MaxTurns    int
	// Workspace, when set, roots the subagent there instead of in the
	// parent's workspace — a git worktree, for parallel work that must not
	// collide. The child's file and shell boundary is that directory.
	Workspace string
}

func (Task) Name() string  { return "task" }
func (Task) Mutates() bool { return false } // the subagent's own tools are policed separately

func (t Task) Description() string {
	types := make([]string, 0, len(t.Profiles))
	for name := range t.Profiles {
		if name != "main" {
			types = append(types, name)
		}
	}
	return "Spawn a subagent with a fresh context to handle a self-contained subtask. " +
		"Use when a task needs extensive exploration whose intermediate detail you do not need — " +
		"the subagent returns only a summary. The prompt must be COMPLETE and self-contained: " +
		"the subagent cannot see this conversation. Agent types: " + strings.Join(types, ", ") + "."
}

func (Task) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "prompt":{"type":"string","description":"Complete, self-contained task description. The subagent sees none of this conversation, so include all necessary context."},
    "description":{"type":"string","description":"3-5 word label shown to the user."},
    "agent_type":{"type":"string","description":"explore | test | review | general. Defaults to general."},
    "max_turns":{"type":"integer","description":"Turn cap for the subagent."}
  },
  "required":["prompt","description"]
}`)
}

// hasTasks reports whether raw carries a tasks field, the tasks tool's argument.
func hasTasks(raw json.RawMessage) bool {
	var probe struct {
		Tasks json.RawMessage `json:"tasks"`
	}
	return json.Unmarshal(raw, &probe) == nil && len(probe.Tasks) > 0 && string(probe.Tasks) != "null"
}

type taskArgs struct {
	Prompt      string `json:"prompt"`
	Description string `json:"description"`
	AgentType   string `json:"agent_type"`
	MaxTurns    int    `json:"max_turns"`
}

func (t Task) Run(ctx context.Context, _ *tools.Session, raw json.RawMessage) tools.Result {
	var a taskArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return tools.Result{Content: fmt.Sprintf("Invalid arguments for task: %v", err), IsError: true}
	}
	if strings.TrimSpace(a.Prompt) == "" {
		// A list of tasks sent here is a call meant for the tasks tool; say so, or the model retries the same call.
		if hasTasks(raw) {
			return tools.Result{Content: "task takes one prompt; to run several subagents at once, call the tasks tool with this tasks list.", IsError: true}
		}
		return tools.Result{Content: "prompt is required and must be self-contained.", IsError: true}
	}
	if strings.TrimSpace(a.Description) == "" {
		return tools.Result{Content: "description is required (3-5 words, shown to the user).", IsError: true}
	}
	agentType := a.AgentType
	if agentType == "" {
		agentType = "general"
	}
	if agentType != "general" {
		if _, found := t.Profiles[agentType]; !found {
			known := make([]string, 0, len(t.Profiles))
			for name := range t.Profiles {
				known = append(known, name)
			}
			return tools.Result{
				Content: fmt.Sprintf("Unknown agent_type %q. Available: %s.", agentType, strings.Join(known, ", ")),
				IsError: true,
			}
		}
	}

	summary, err := t.Spawn(ctx, SubagentRequest{
		Prompt:      a.Prompt,
		Description: a.Description,
		AgentType:   agentType,
		MaxTurns:    a.MaxTurns,
	})
	if err != nil {
		return tools.Result{Content: err.Error(), IsError: true}
	}
	return tools.Result{Content: summary}
}

// SubagentFactory builds and runs subagents. A subagent answers to the approver
// of the loop that spawned it; Approver serves a spawn with no loop, nil refuses.
type SubagentFactory struct {
	Adapter   model.Adapter
	Tools     *tools.Registry
	Policy    *policy.Engine
	Approver  Approver
	Session   *tools.Session
	Store     Store
	Budget    *Budget
	Config    Config
	Workspace string
	// Redact is handed to every subagent's recorder, so a secret is stopped
	// before a child's record as it is before the parent's.
	Redact Redactor
	// Depth guards against runaway recursion; nested spawning is off by default.
	Depth int
}

// sessionCreator is implemented by durable stores that need a session row
// before events can reference it. The memory store does not implement it, so
// the local path is unaffected.
type sessionCreator interface {
	CreateSubSession(ctx context.Context, id, description string) error
}

// MaxSummaryChars bounds what a subagent returns to its parent. The point of
// delegation is that the parent's context grows by a summary, so an unbounded
// return value defeats the mechanism.
const MaxSummaryChars = 8000

func (f *SubagentFactory) Spawn(ctx context.Context, req SubagentRequest) (string, error) {
	parent, _ := ctx.Value(parentKey{}).(*parentLink)
	depth := f.Depth
	if parent != nil {
		depth += parent.depth
	}
	if f.Budget != nil {
		if !f.Budget.AllowNested && depth > 0 {
			return "", fmt.Errorf(
				"nested subagents are disabled. Do this work directly rather than delegating again")
		}
		if err := f.Budget.TryReserveSubagent(); err != nil {
			return "", fmt.Errorf("cannot spawn subagent: %w. Complete the task with the context you have", err)
		}
	}

	sessionID := newID()
	// A durable store requires the session row before any event references it.
	// Without this a subagent's first event fails the foreign key and the whole
	// delegation errors out — which only shows up once Postgres is configured.
	if creator, ok := f.Store.(sessionCreator); ok {
		if err := creator.CreateSubSession(ctx, sessionID, req.Description); err != nil {
			return "", fmt.Errorf("could not record subagent session: %w", err)
		}
	}
	// The child asks whoever the parent asks: the person at the prompt, or the
	// headless refuser. Nothing it does is approved on its behalf.
	approver, parentID := f.Approver, ""
	if parent != nil {
		inner := parent.approver
		if o, nested := inner.(oneAtATime); nested {
			inner = o.Approver // one queue for the whole tree, never taken twice
		}
		approver = oneAtATime{Approver: inner, asks: parent.asks, who: req.Description}
		if parent.rec == nil {
			parent = nil
		} else {
			parentID = parent.rec.sessionID
		}
	}
	if approver == nil {
		approver = AutoApprove{Yes: false}
	}
	rec := NewRecorder(f.Store, sessionID, parentID)
	rec.Redact = f.Redact
	if parent != nil {
		rec.tap = mirrorInto(parent, sessionID)
	}

	profile := req.AgentType
	if _, found := Profiles[profile]; !found {
		profile = "main"
	}

	// Where the subagent works. Usually the parent's workspace and session;
	// for isolated parallel work, its own worktree with its own scoping
	// boundary, so two children cannot write over each other and neither can
	// reach the parent's tree.
	workspace, session := f.Workspace, f.Session
	if req.Workspace != "" {
		var err error
		if session, err = tools.NewSession(req.Workspace); err != nil {
			return "", fmt.Errorf("subagent workspace: %w", err)
		}
		if f.Session != nil {
			session.Syntax = f.Session.Syntax
		}
		workspace = req.Workspace
	}

	// Fresh context: the subagent gets its own system prompt and memory file,
	// and none of the parent's turns.
	sysPrompt := BuildSystemPrompt(BuildOptions{
		Profile:       profile,
		Workspace:     workspace,
		Model:         f.Adapter.Profile().Name,
		ContextWindow: f.Adapter.Profile().ContextWindow,
		MemoryFiles:   DiscoverMemoryFiles(workspace),
	})

	// A narrow role gets a narrow tool set: an explore subagent that can write
	// will write, and the orchestrator will not know (docs §07).
	registry := f.Tools
	if p, found := Profiles[profile]; found && len(p.Tools) > 0 {
		registry = f.Tools.Subset(p.Tools...)
	}

	cfg := f.Config
	cfg.SystemPrompt = sysPrompt
	if req.MaxTurns > 0 {
		cfg.MaxTurns = req.MaxTurns
	} else if cfg.MaxTurns > 30 {
		cfg.MaxTurns = 30 // subagents are for bounded subtasks
	}

	sub := NewLoop(f.Adapter, registry, childPolicy(f.Policy, session), approver, session, rec, cfg)
	sub.depth = depth + 1
	// Deliberately no Compactor: a subagent that needs compaction was given too
	// large a task, and silently compacting hides that from the operator.

	// The parent records the spawn and the return in its own log, which is
	// what the audit relies on; the child's copy is for its own replay.
	spawned := map[string]any{
		"description": req.Description,
		"agent_type":  req.AgentType,
		"depth":       depth,
		"workspace":   workspace,
		"session":     sessionID,
	}
	_, _ = rec.Record(EvSubagentSpawned, ActorAgent, Trusted, spawned)
	parent.record(EvSubagentSpawned, ActorAgent, spawned)

	reason, err := sub.Run(ctx, req.Prompt)
	usage := sub.Usage()
	f.Budget.Spend(usage.InputTokens + usage.OutputTokens)

	if err != nil {
		parent.record(EvSubagentReturn, ActorAgent, map[string]any{
			"description": req.Description, "session": sessionID, "reason": string(TermError),
			"turns": usage.Turns, "tokens_in": usage.InputTokens, "tokens_out": usage.OutputTokens,
		})
		return "", fmt.Errorf("subagent failed: %w", err)
	}

	summary := lastAssistantMessage(sub.Messages())
	if strings.TrimSpace(summary) == "" {
		summary = fmt.Sprintf("(subagent ended with %s and produced no summary)", reason)
	}
	if len(summary) > MaxSummaryChars {
		summary = summary[:MaxSummaryChars] + "\n\n[summary truncated]"
	}

	returned := map[string]any{
		"description":   req.Description,
		"session":       sessionID,
		"reason":        string(reason),
		"turns":         usage.Turns,
		"tokens_in":     usage.InputTokens,
		"tokens_out":    usage.OutputTokens,
		"summary_chars": len(summary),
	}
	_, _ = rec.Record(EvSubagentReturn, ActorAgent, Trusted, returned)
	parent.record(EvSubagentReturn, ActorAgent, returned)

	if reason != TermCompleted {
		return summary + fmt.Sprintf("\n\n[subagent ended early: %s]", reason), nil
	}
	return summary, nil
}

// parentKey carries the loop running a tool, so a subagent that tool spawns
// answers to the same approver and is linked into the same record.
type parentKey struct{}

type parentLink struct {
	approver Approver
	rec      *Recorder
	asks     chan struct{} // one ask at a time across the whole tree under one loop
	depth    int           // 0 for a top-level loop, 1 for its subagents, and so on
	fail     func(error)   // a write the parent's record refused ends the parent's run
}

// asParent marks ctx as coming from this loop, for the subagents a tool spawns.
func (l *Loop) asParent(ctx context.Context) context.Context {
	l.asksOnce.Do(func() { l.asks = make(chan struct{}, 1) })
	asks := l.asks
	if p, ok := ctx.Value(parentKey{}).(*parentLink); ok {
		asks = p.asks
	}
	return context.WithValue(ctx, parentKey{}, &parentLink{
		approver: l.Approver, rec: l.Recorder, asks: asks, depth: l.depth, fail: l.noteRecordErr,
	})
}

// record writes to the parent's record; a nil link has none.
func (p *parentLink) record(t EventType, actor Actor, payload any) {
	if p == nil {
		return
	}
	if _, err := p.rec.Record(t, actor, Trusted, payload); err != nil && p.fail != nil {
		p.fail(err)
	}
}

// oneAtATime serializes asks: sibling subagents run together, but a person
// answers one prompt at a time. A cancelled wait gives up without asking.
type oneAtATime struct {
	Approver
	asks chan struct{}
	who  string
}

func (o oneAtATime) Approve(ctx context.Context, tool string, args json.RawMessage, res policy.Result) (bool, error) {
	select {
	case o.asks <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-o.asks }()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return o.Approver.Approve(WithSubagent(ctx, o.who), tool, args, res)
}

type subagentKey struct{}

// WithSubagent names the subagent an ask comes from, for the prompt to show.
func WithSubagent(ctx context.Context, description string) context.Context {
	return context.WithValue(ctx, subagentKey{}, description)
}

// SubagentOf is the subagent an ask comes from, or "" for the loop's own.
func SubagentOf(ctx context.Context) string {
	s, _ := ctx.Value(subagentKey{}).(string)
	return s
}

// SubagentAction is a subagent's call that was refused or put to an approver,
// copied into the parent's record; the rest stay in the record Session names.
type SubagentAction struct {
	Session      string `json:"session"`
	CallID       string `json:"call_id"`
	Tool         string `json:"tool"`
	Subject      string `json:"subject,omitempty"`
	Decision     string `json:"decision"` // allowed | denied
	Step         string `json:"step,omitempty"`
	Reason       string `json:"reason,omitempty"`
	By           string `json:"by,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Approver     string `json:"approver,omitempty"`
	GrantedScope string `json:"granted_scope,omitempty"`
}

// mirrorInto copies the child's settled calls that matter to an audit into
// the parent's record as they happen.
func mirrorInto(parent *parentLink, child string) func(Event) {
	var mu sync.Mutex
	asked := map[string]ActionRequested{}
	return func(ev Event) {
		switch ev.Type {
		case EvSubagentSpawned, EvSubagentReturn, EvSubagentAction:
			// A nested subagent's events are passed up, so the root record has them;
			// the child's own spawn and return are written to the parent directly.
			var own struct {
				Session string `json:"session"`
			}
			if ev.Type != EvSubagentAction && json.Unmarshal(ev.Payload, &own) == nil && own.Session == child {
				return
			}
			parent.record(ev.Type, ev.Actor, ev.Payload)
		case EvActionRequested:
			var a ActionRequested
			if json.Unmarshal(ev.Payload, &a) == nil {
				mu.Lock()
				asked[a.CallID] = a
				mu.Unlock()
			}
		case EvActionApproved, EvActionDenied:
			var d map[string]string
			_ = json.Unmarshal(ev.Payload, &d)
			mu.Lock()
			a, ok := asked[d["call_id"]]
			delete(asked, d["call_id"])
			mu.Unlock()
			decision := "denied"
			if ev.Type == EvActionApproved {
				decision = "allowed"
			}
			if !ok || (decision == "allowed" && d["by"] == ByPolicy) {
				return
			}
			parent.record(EvSubagentAction, ev.Actor, SubagentAction{
				Session: child, CallID: a.CallID, Tool: a.Tool, Subject: policy.Subject(a.Tool, a.Args),
				Decision: decision, Step: d["step"], Reason: d["reason"], By: d["by"],
				Scope: d["scope"], Approver: d["approver"], GrantedScope: d["granted_scope"],
			})
		}
	}
}

func lastAssistantMessage(msgs []model.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleAssistant && strings.TrimSpace(msgs[i].Content) != "" {
			return msgs[i].Content
		}
	}
	return ""
}

// childPolicy is the parent's policy with the child's roots added, so a path
// rule relative to the workspace also matches inside the child's worktree.
func childPolicy(pol *policy.Engine, session *tools.Session) *policy.Engine {
	if pol == nil || session == nil {
		return pol
	}
	child, parent := *pol, pol.Roots
	child.Roots = func() []string {
		roots := session.PolicyRoots()
		if parent != nil {
			roots = append(roots, parent()...)
		}
		return roots
	}
	return &child
}
