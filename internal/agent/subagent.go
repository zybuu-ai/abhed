package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// SubagentFactory builds and runs subagents. It owns the wiring a subagent
// needs so the Task tool stays a thin adapter.
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
	Redact func([]byte) []byte
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
	if f.Budget != nil {
		if !f.Budget.AllowNested && f.Depth > 0 {
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
	rec := NewRecorder(f.Store, sessionID, "")
	rec.Redact = f.Redact

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

	sub := NewLoop(f.Adapter, registry, f.Policy, f.Approver, session, rec, cfg)
	// Deliberately no Compactor: a subagent that needs compaction was given too
	// large a task, and silently compacting hides that from the operator.

	// The parent records the spawn and the return in its own log, which is
	// what the audit relies on; the child's copy is for its own replay.
	_, _ = rec.Record(EvSubagentSpawned, ActorAgent, Trusted, map[string]any{
		"description": req.Description,
		"agent_type":  req.AgentType,
		"depth":       f.Depth,
		"workspace":   workspace,
	})

	reason, err := sub.Run(ctx, req.Prompt)
	usage := sub.Usage()
	f.Budget.Spend(usage.InputTokens + usage.OutputTokens)

	if err != nil {
		return "", fmt.Errorf("subagent failed: %w", err)
	}

	summary := lastAssistantMessage(sub.Messages())
	if strings.TrimSpace(summary) == "" {
		summary = fmt.Sprintf("(subagent ended with %s and produced no summary)", reason)
	}
	if len(summary) > MaxSummaryChars {
		summary = summary[:MaxSummaryChars] + "\n\n[summary truncated]"
	}

	_, _ = rec.Record(EvSubagentReturn, ActorAgent, Trusted, map[string]any{
		"description":   req.Description,
		"reason":        string(reason),
		"turns":         usage.Turns,
		"tokens_in":     usage.InputTokens,
		"tokens_out":    usage.OutputTokens,
		"summary_chars": len(summary),
	})

	if reason != TermCompleted {
		return summary + fmt.Sprintf("\n\n[subagent ended early: %s]", reason), nil
	}
	return summary, nil
}

func lastAssistantMessage(msgs []model.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleAssistant && strings.TrimSpace(msgs[i].Content) != "" {
			return msgs[i].Content
		}
	}
	return ""
}
