package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
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
	// Agents are the agent types this session offers; nil offers the
	// built-in roles. They are fixed for the session, since they are part of
	// the prompt the model was given.
	Agents *Definitions
	// Workspace is the parent's root, beneath which a role that works in its
	// own worktree gets one.
	Workspace string
	// Models are the provider names a call may choose. The model property is
	// offered only when there is more than one to choose from.
	Models []string
	// Background starts a child that outlives the call; nil offers no
	// background property.
	Background func(ctx context.Context, req SubagentRequest) (string, error)
}

type SubagentRequest struct {
	Prompt      string
	Description string
	AgentType   string
	MaxTurns    int
	// Model names a configured provider to run on, over the definition's;
	// empty or "inherit" takes the definition's, then the parent's model.
	Model string
	// Workspace, when set, roots the subagent there instead of in the
	// parent's workspace — a git worktree, for parallel work that must not
	// collide. The child's file and shell boundary is that directory.
	Workspace string

	// sessionID, when set, is the child's session id, chosen by the caller.
	sessionID string
	// settle, when set, runs once a background child has ended and returns
	// what its notice says about the worktree it worked in.
	settle func(context.Context) string
}

func (Task) Name() string  { return "task" }
func (Task) Mutates() bool { return false } // the subagent's own tools are policed separately

// MutatesCall is true for a role that works in its own worktree, which makes
// a branch and a checkout on the host before the child runs.
func (t Task) MutatesCall(raw json.RawMessage) bool {
	var a taskArgs
	if json.Unmarshal(raw, &a) != nil {
		return false
	}
	def, ok := t.Agents.Get(a.AgentType)
	return ok && def.Isolation == "worktree"
}

func (t Task) Description() string {
	note := ""
	if t.Background != nil {
		note = backgroundNote
	}
	return "Spawn a subagent with a fresh context to handle a self-contained subtask. " +
		"Use when a task needs extensive exploration whose intermediate detail you do not need — " +
		"the subagent returns only a summary. The prompt must be COMPLETE and self-contained: " +
		"the subagent cannot see this conversation." + note + " Agent types:" + t.Agents.listing()
}

func (t Task) Schema() json.RawMessage {
	return mustSchema(map[string]any{
		"type": "object",
		"properties": withModel(map[string]any{
			"prompt":      map[string]any{"type": "string", "description": "Complete, self-contained task description. The subagent sees none of this conversation, so include all necessary context."},
			"description": map[string]any{"type": "string", "description": "3-5 word label shown to the user."},
			"agent_type":  agentTypeSchema(t.Agents),
			"max_turns":   map[string]any{"type": "integer", "description": "Turn cap for the subagent."},
		}, t.Models, t.Background != nil),
		"required": []string{"prompt", "description"},
	})
}

// withModel adds the model property when there is a choice to make. With one
// model configured it would only cost prompt tokens.
func withModel(props map[string]any, models []string, background bool) map[string]any {
	if background {
		props["background"] = map[string]any{"type": "boolean",
			"description": "Start it and continue at once; its result is delivered to you automatically."}
	}
	if len(models) > 1 {
		props["model"] = map[string]any{"type": "string", "enum": models,
			"description": "A configured model to run the subagent on. Omit to use the agent type's, or yours."}
	}
	return props
}

// agentTypeSchema is the agent_type property: the types this session offers.
func agentTypeSchema(d *Definitions) map[string]any {
	return map[string]any{"type": "string", "enum": d.Names(),
		"description": "One of the agent types listed on the task tool. Defaults to general."}
}

func mustSchema(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // built from fixed shapes and strings; cannot fail
	}
	return b
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
	Model       string `json:"model"`
	Background  bool   `json:"background"`
}

// unknownType refuses an agent type the session does not offer, naming those it does.
func unknownType(d *Definitions, what, name string) tools.Result {
	return tools.Result{
		Content: fmt.Sprintf("Unknown %s %q. Available: %s.", what, name, strings.Join(d.Names(), ", ")),
		IsError: true,
	}
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
	def, found := t.Agents.Get(agentType)
	if !found {
		return unknownType(t.Agents, "agent_type", agentType)
	}
	req := SubagentRequest{
		Prompt:      a.Prompt,
		Description: a.Description,
		AgentType:   agentType,
		MaxTurns:    a.MaxTurns,
		Model:       a.Model,
	}
	if a.Background {
		if t.Background == nil {
			return tools.Result{Content: "this agent runs no background tasks; call task without background.", IsError: true}
		}
		if def.Isolation == "worktree" {
			wt, res := makeWorktree(ctx, t.Workspace, req.AgentType)
			if wt == nil {
				return res
			}
			req.Workspace, req.settle = wt.Dir, settleLater(t.Workspace, wt)
		}
		id, err := t.Background(ctx, req)
		if err != nil {
			if req.settle != nil {
				req.settle(context.WithoutCancel(ctx))
			}
			return tools.Result{Content: err.Error(), IsError: true}
		}
		return tools.Result{Content: startedText(id, req.Description)}
	}
	if def.Isolation == "worktree" {
		return runInWorktree(ctx, t.Spawn, t.Workspace, req)
	}
	summary, err := t.Spawn(ctx, req)
	if err != nil {
		return tools.Result{Content: err.Error(), IsError: true}
	}
	return tools.Result{Content: summary}
}

// makeWorktree makes a worktree for a role that works in one, or says why not.
func makeWorktree(ctx context.Context, ws, agentType string) (*worktree, tools.Result) {
	if err := requireGitRepo(ctx, ws); err != nil {
		return nil, tools.Result{Content: fmt.Sprintf("agent type %q works in its own worktree, which needs the workspace to be a git repository: %v", agentType, err), IsError: true}
	}
	wt, err := addWorktree(ctx, ws)
	if err != nil {
		return nil, tools.Result{Content: "could not create a worktree: " + err.Error(), IsError: true}
	}
	return wt, tools.Result{}
}

// settleLater settles a background child's worktree when the child ends.
func settleLater(ws string, wt *worktree) func(context.Context) string {
	return func(ctx context.Context) string {
		rel, _ := filepath.Rel(ws, wt.Dir)
		return settleWorktree(ctx, ws, rel, wt)
	}
}

// runInWorktree runs one subagent in a worktree of its own and says what it
// left there, for a role whose definition works in one.
func runInWorktree(ctx context.Context, spawn func(context.Context, SubagentRequest) (string, error), ws string, req SubagentRequest) tools.Result {
	wt, res := makeWorktree(ctx, ws, req.AgentType)
	if wt == nil {
		return res
	}
	req.Workspace = wt.Dir
	summary, err := spawn(ctx, req)
	rel, _ := filepath.Rel(ws, wt.Dir)
	settled := settleWorktree(ctx, ws, rel, wt)
	if err != nil {
		return tools.Result{Content: err.Error() + "\n\n" + settled, IsError: true}
	}
	return tools.Result{Content: strings.TrimSpace(summary) + "\n\n" + settled}
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
	// Definitions are the agent types a spawn may name; nil offers the
	// built-in roles. The task and tasks tools offer the same set.
	Definitions *Definitions
	// Models turns a configured provider name into an adapter, for a child
	// run on another model than its parent's. It must look the name up among
	// the providers offered to this session and never treat it as an
	// endpoint. Nil offers no choice: only the parent's model.
	Models func(name string) (model.Adapter, error)
	// ModelNames are the names Models resolves, as the tools offer them.
	ModelNames []string
	// Background offers background tasks: the task and tasks tools take a
	// background flag, and task_status and task_cancel are offered. The
	// loop that runs them needs a Background manager.
	Background bool
}

// SessionCreator is implemented by durable stores that need a session row
// before events can reference it. The memory store does not implement it, so
// the local path is unaffected. parentID is the session that spawned it.
type SessionCreator interface {
	CreateSubagentSession(ctx context.Context, id, parentID, description string) error
}

// MaxSummaryChars bounds what a subagent returns to its parent. The point of
// delegation is that the parent's context grows by a summary, so an unbounded
// return value defeats the mechanism.
const MaxSummaryChars = 8000

func (f *SubagentFactory) Spawn(ctx context.Context, req SubagentRequest) (string, error) {
	c, err := f.prepare(ctx, req, nil, nil)
	if err != nil {
		return "", err
	}
	summary, _, err := c.execute(ctx)
	return summary, err
}

// child is a subagent ready to run: its loop is built and its spawn recorded.
type child struct {
	sub       *Loop
	parent    *parentLink
	req       SubagentRequest
	sessionID string
	adapter   model.Adapter
	provider  string
	// extra goes on both its spawned and returned events, such as a
	// background task's id.
	extra map[string]any
}

// prepare settles everything a spawn needs and records it. reserve runs just
// before the spawn is counted, and may refuse it.
func (f *SubagentFactory) prepare(ctx context.Context, req SubagentRequest, extra map[string]any, reserve func() error) (*child, error) {
	parent, _ := ctx.Value(parentKey{}).(*parentLink)
	depth, adapter := f.Depth, f.Adapter
	if parent != nil {
		depth += parent.depth
		// A subagent runs on the model its parent runs on now, not the one at startup.
		if parent.adapter != nil {
			adapter = parent.adapter
		}
	}
	// The role and its tools are settled before a spawn is counted: a type
	// this session does not offer, or a tool list naming a tool it does not
	// have, refuses the call with nothing spawned.
	def, found := f.Definitions.Get(req.AgentType)
	if !found {
		return nil, fmt.Errorf("unknown agent type %q; available: %s", req.AgentType, strings.Join(f.Definitions.Names(), ", "))
	}
	registry, err := childTools(f.Tools, def)
	if err != nil {
		return nil, err
	}
	// The model is resolved before a spawn is counted too, and a model that
	// cannot be had refuses the spawn: a child never runs on another model
	// than the one named, and the parent's is never substituted.
	provider := ""
	if parent != nil {
		provider = parent.provider
	}
	// The organisation's choice of model for a managed role binds, as its
	// turn cap does: a call cannot move the role to another provider.
	if def.Source == SourceManaged && def.Model != "" && childModel(req.Model, "") != "" && req.Model != def.Model {
		return nil, fmt.Errorf("agent type %s runs on model %q, set by the organisation; omit model", def.Name, def.Model)
	}
	if name := childModel(req.Model, def.Model); name != "" {
		if err := ValidModelName(name); err != nil {
			return nil, err
		}
		if f.Models == nil {
			return nil, fmt.Errorf("model %q was asked for, and this agent offers no model choice; omit model to use yours", name)
		}
		a, err := f.Models(name)
		if err != nil {
			return nil, fmt.Errorf("model %q is not available: %w", name, err)
		}
		adapter, provider = a, name
	}
	if f.Budget != nil && !f.Budget.AllowNested && depth > 0 {
		return nil, fmt.Errorf(
			"nested subagents are disabled. Do this work directly rather than delegating again")
	}
	if reserve != nil {
		if err := reserve(); err != nil {
			return nil, err
		}
	}
	if f.Budget != nil {
		if err := f.Budget.TryReserveSubagent(); err != nil {
			return nil, fmt.Errorf("cannot spawn subagent: %w. Complete the task with the context you have", err)
		}
	}

	sessionID := req.sessionID
	if sessionID == "" {
		sessionID = newID()
	}
	// A durable store requires the session row before any event references it.
	// Without this a subagent's first event fails the foreign key and the whole
	// delegation errors out — which only shows up once Postgres is configured.
	if creator, ok := f.Store.(SessionCreator); ok {
		parentSession := ""
		if parent != nil && parent.rec != nil {
			parentSession = parent.rec.sessionID
		}
		if err := creator.CreateSubagentSession(ctx, sessionID, parentSession, req.Description); err != nil {
			return nil, fmt.Errorf("could not record subagent session: %w", err)
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
	// A child redacts as its parent's session does; the factory's own is for a
	// spawn with no parent.
	rec.Redact = f.Redact
	if parent != nil && parent.rec.redactor() != nil {
		rec.Redact = parent.rec.redactor()
	}
	if parent != nil {
		rec.tap = mirrorInto(parent, sessionID, req.Description)
	}

	profile, role := "main", ""
	if def.Source == SourceBuiltin {
		if def.Name != "general" {
			profile = def.Name
		}
	} else {
		role = def.Instruction
	}

	// Where the subagent works. Usually the parent's workspace and session;
	// for isolated parallel work, its own worktree with its own scoping
	// boundary, so two children cannot write over each other and neither can
	// reach the parent's tree.
	workspace, session := f.Workspace, f.Session
	if req.Workspace != "" {
		if session, err = tools.NewSession(req.Workspace); err != nil {
			return nil, fmt.Errorf("subagent workspace: %w", err)
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
		Role:          role,
		Workspace:     workspace,
		Model:         adapter.Profile().Name,
		ContextWindow: adapter.Profile().ContextWindow,
		MemoryFiles:   DiscoverMemoryFiles(workspace),
	})

	cfg := f.Config
	cfg.SystemPrompt = sysPrompt
	cfg.MaxTurns = childTurns(f.Config.MaxTurns, def.MaxTurns, req.MaxTurns)

	sub := NewLoop(adapter, registry, narrowMode(childPolicy(f.Policy, session), def.PermissionMode), approver, session, rec, cfg)
	// recall is added by NewLoop to every loop; a role that disallows it
	// does without, as with any other tool.
	if def.strict && len(def.DisallowedTools) > 0 {
		sub.Tools = sub.Tools.Without(def.DisallowedTools)
	}
	sub.depth = depth + 1
	sub.Provider = provider
	// The child spends from the parent's allowance turn by turn, so it stops
	// when the session's budget runs out rather than after it.
	sub.Budget = f.Budget
	// Deliberately no Compactor: a subagent that needs compaction was given too
	// large a task, and silently compacting hides that from the operator.

	// The parent records the spawn and the return in its own log, which is
	// what the audit relies on; the child's copy is for its own replay.
	effective := registry.Names()
	sort.Strings(effective)
	spawned := map[string]any{
		"description":       req.Description,
		"agent_type":        req.AgentType,
		"depth":             depth,
		"workspace":         workspace,
		"session":           sessionID,
		"definition":        def.Name,
		"definition_source": def.Source,
		"tools":             effective,
		"model":             adapter.Profile().Name,
	}
	if def.SHA256 != "" {
		spawned["definition_sha256"] = def.SHA256
	}
	if provider != "" {
		spawned["provider"] = provider
	}
	for k, v := range extra {
		spawned[k] = v
	}
	_, _ = rec.Record(EvSubagentSpawned, ActorAgent, Trusted, spawned)
	parent.record(EvSubagentSpawned, ActorAgent, spawned)
	return &child{sub: sub, parent: parent, req: req, sessionID: sessionID,
		adapter: adapter, provider: provider, extra: extra}, nil
}

// execute runs a prepared child to its end, records its return in both
// records, and gives back its summary as the parent should read it.
func (c *child) execute(ctx context.Context) (string, TerminalReason, error) {
	reason, err := c.sub.Run(ctx, c.req.Prompt)
	usage := c.sub.Usage()
	returned := map[string]any{
		"description": c.req.Description,
		"session":     c.sessionID,
		"reason":      string(reason),
		"turns":       usage.Turns,
		"tokens_in":   usage.InputTokens,
		"tokens_out":  usage.OutputTokens,
		"model":       c.adapter.Profile().Name,
	}
	if c.provider != "" {
		returned["provider"] = c.provider
	}
	for k, v := range c.extra {
		returned[k] = v
	}

	if err != nil {
		returned["reason"] = string(TermError)
		c.parent.record(EvSubagentReturn, ActorAgent, returned)
		return "", TermError, fmt.Errorf("subagent failed: %w", err)
	}

	summary := lastAssistantMessage(c.sub.Messages())
	if strings.TrimSpace(summary) == "" {
		summary = fmt.Sprintf("(subagent ended with %s and produced no summary)", reason)
	}
	if len(summary) > MaxSummaryChars {
		summary = summary[:MaxSummaryChars] + "\n\n[summary truncated]"
	}
	returned["summary_chars"] = len(summary)
	_, _ = c.sub.Recorder.Record(EvSubagentReturn, ActorAgent, Trusted, returned)
	c.parent.record(EvSubagentReturn, ActorAgent, returned)

	if reason != TermCompleted {
		return summary + fmt.Sprintf("\n\n[subagent ended early: %s]", reason), reason, nil
	}
	return summary, reason, nil
}

// childTools is the tool set a role gets, cut from the parent's registry at
// the moment of the spawn. A narrow role gets a narrow tool set: an explore
// subagent that can write will write, and the orchestrator will not know
// (docs §07). A loaded definition's list may only narrow, and a tool it names
// that the session does not have refuses the spawn rather than being dropped.
func childTools(parent *tools.Registry, def *Definition) (*tools.Registry, error) {
	if parent == nil {
		parent = tools.NewRegistry()
	}
	if !def.strict {
		if len(def.Tools) > 0 {
			return parent.Subset(def.Tools...), nil
		}
		return parent, nil
	}
	reg := parent
	if def.Tools != nil {
		// recall is every loop's own, bound to its record, and never in the
		// parent's registry; naming it asks for nothing the child lacks.
		names := make([]string, 0, len(def.Tools))
		for _, n := range def.Tools {
			if !strings.EqualFold(n, "recall") {
				names = append(names, n)
			}
		}
		var missing []string
		if reg, missing = parent.SubsetStrict(names); len(missing) > 0 {
			return nil, fmt.Errorf("definition %s names tools this session does not have: %s. Use another agent type, or do the work directly",
				def.Name, strings.Join(missing, ", "))
		}
	}
	if len(def.DisallowedTools) > 0 {
		reg = reg.Without(def.DisallowedTools)
	}
	return reg, nil
}

// childModel is the model a child is asked to run on: the call's, else the
// definition's. Empty, or inherit, is the parent's.
func childModel(call, def string) string {
	for _, m := range []string{call, def} {
		if m != "" && m != "inherit" {
			return m
		}
	}
	return ""
}

// ValidModelName refuses a model value that is not a plain provider name. A
// URL, a path or anything with spaces is never looked up, so no value can
// point a subagent at an endpoint of its own.
func ValidModelName(v string) error {
	if v == "" || len(v) > 128 || strings.Contains(v, "://") || strings.ContainsAny(v, "/\\ \t\r\n") {
		return fmt.Errorf("model %q is not a provider name; name a configured provider, never an endpoint", v)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("model %q is not a provider name", v)
		}
	}
	return nil
}

// childTurns is a child's turn cap: the call's, else the definition's, else
// thirty, since subagents are for bounded subtasks. A definition's cap binds
// the call, and nothing goes above the parent's cap.
func childTurns(parent, def, call int) int {
	limit := parent
	if def > 0 && (limit <= 0 || def < limit) {
		limit = def
	}
	turns := 30
	switch {
	case call > 0:
		turns = call
	case def > 0:
		turns = def
	}
	if limit > 0 && turns > limit {
		turns = limit
	}
	return turns
}

// modeRank orders the permission modes from narrowest to widest; a mode not
// listed ranks widest, so a definition's mode always narrows it.
var modeRank = map[policy.Mode]int{policy.ModePlan: 0, policy.ModeDefault: 1, policy.ModeAcceptEdits: 2, policy.ModeAuto: 3, policy.ModeBypass: 4}

func rankOf(m policy.Mode) int {
	if r, ok := modeRank[m]; ok {
		return r
	}
	return len(modeRank)
}

// narrowMode is the child's policy with a definition's mode where it is
// narrower than the parent's, on a copy of the engine: its deny rules, hooks
// and managed marking come along, and the parent's engine is untouched. A
// mode that would widen is ignored.
func narrowMode(pol *policy.Engine, mode string) *policy.Engine {
	if pol == nil || mode == "" || rankOf(policy.Mode(mode)) >= rankOf(pol.Mode) {
		return pol
	}
	child := *pol
	child.Mode = policy.Mode(mode)
	return &child
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
	adapter  model.Adapter // the parent's model now, which a switch may have changed
	provider string        // the configured name adapter came from, when known
	loop     *Loop         // the loop making the call, which a pipeline's steps run on
}

// asParent marks ctx as coming from this loop, for the subagents a tool spawns.
func (l *Loop) asParent(ctx context.Context) context.Context {
	asks := l.askQueue(ctx)
	return context.WithValue(ctx, parentKey{}, &parentLink{
		approver: l.Approver, rec: l.Recorder, asks: asks, depth: l.depth, fail: l.noteRecordErr,
		adapter: l.Adapter, provider: l.Provider, loop: l,
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
	via  string // the pipeline asking, when a pipeline step asks
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
	if o.who != "" {
		ctx = WithSubagent(ctx, o.who)
	}
	if o.via != "" {
		ctx = context.WithValue(ctx, pipelineAskKey{}, o.via)
	}
	return o.Approver.Approve(ctx, tool, args, res)
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
	// RequestID is the child's action.requested event, which a subagent.ask
	// for the same call names too.
	RequestID string `json:"request_id,omitempty"`
}

// SubagentAsk is a subagent's call put to the approver, copied into the
// parent's record before the approver is asked. RequestID is the id an answer
// names, as for the parent's own asks.
type SubagentAsk struct {
	Session   string          `json:"session"`
	Subagent  string          `json:"subagent,omitempty"`
	RequestID string          `json:"request_id"`
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	Subject   string          `json:"subject,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Scope     string          `json:"scope,omitempty"`
	Via       string          `json:"via,omitempty"`
}

// mirrorInto copies the child's settled calls that matter to an audit into
// the parent's record as they happen.
func mirrorInto(parent *parentLink, child, description string) func(Event) {
	var mu sync.Mutex
	type request struct {
		ActionRequested
		id string
	}
	asked := map[string]request{}
	return func(ev Event) {
		switch ev.Type {
		case EvSubagentSpawned, EvSubagentReturn, EvSubagentAction, EvSubagentAsk:
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
				asked[a.CallID] = request{a, ev.ID}
				mu.Unlock()
				// Written before the approver is asked, so a console or editor
				// watching the parent can show the request it is waiting on.
				if a.RequiresApproval {
					parent.record(EvSubagentAsk, ev.Actor, SubagentAsk{
						Session: child, Subagent: description, RequestID: ev.ID, CallID: a.CallID,
						Tool: a.Tool, Args: a.Args, Subject: policy.Subject(a.Tool, a.Args),
						Reason: a.Reason, Scope: a.Scope, Via: a.Via,
					})
				}
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
				RequestID: a.id,
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
