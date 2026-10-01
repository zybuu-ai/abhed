package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/model"
)

// Resuming a finished subagent continues its own conversation with a new
// instruction: the same session id and record, the same role and model, and
// the same worktree. Only the session that started it may, and only once it
// has ended.

// SubSessionChecker is implemented by stores that know which session a
// child belongs to, and whose, so a resume is refused for any other.
type SubSessionChecker interface {
	SubSessionOf(ctx context.Context, childID, parentID string) (bool, error)
}

// errNoSuchTask answers every resume that may not go ahead because of whose
// task it is or whether it exists, the same way, so it tells nothing.
func errNoSuchTask(id string) error {
	return fmt.Errorf("no such task %q to resume in this session", id)
}

// resuming holds the children being resumed in this process, so one cannot
// be resumed twice at once.
var resuming sync.Map

// spawnedRecord is what a child's own subagent.spawned says about it.
type spawnedRecord struct {
	Description string `json:"description"`
	AgentType   string `json:"agent_type"`
	Depth       int    `json:"depth"`
	Workspace   string `json:"workspace"`
	Definition  string `json:"definition"`
	SHA256      string `json:"definition_sha256"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Branch      string `json:"branch"`
	Start       string `json:"start"`
}

// prepareResume settles a resume: whose child it is, that it has ended, its
// role and model as they are now, its worktree; then it rebuilds the child's
// conversation on a loop that goes on in the child's own record.
func (f *SubagentFactory) prepareResume(ctx context.Context, req SubagentRequest, extra map[string]any, reserve func() error) (*child, error) {
	id := req.Resume
	parent, _ := ctx.Value(parentKey{}).(*parentLink)
	if parent == nil || parent.rec == nil || f.Store == nil || !validTaskID(id) {
		return nil, errNoSuchTask(id)
	}
	events, err := f.Store.Events(id)
	// Only the direct parent: the child's record names it, and a store that
	// knows owners agrees. Anything else, a grandchild included, is "no such task".
	if err != nil || len(events) == 0 || events[0].ParentID != parent.rec.sessionID {
		return nil, errNoSuchTask(id)
	}
	if checker, ok := f.Store.(SubSessionChecker); ok {
		if mine, err := checker.SubSessionOf(ctx, id, parent.rec.sessionID); err != nil || !mine {
			return nil, errNoSuchTask(id)
		}
	}
	// Still running, in this session's background or being resumed now: refused.
	if parent.loop != nil {
		if t, ok := parent.loop.Background.Task(id); ok && t.Status == "running" {
			return nil, errNoSuchTask(id)
		}
	}
	// Any recorded end will do: finished, capped, or stopped from outside
	// (lost, shutdown, deadline, session_closed, user_interrupt).
	end, ended := LastEnd(events)
	if !ended {
		return nil, errNoSuchTask(id)
	}
	if _, busy := resuming.LoadOrStore(id, true); busy {
		return nil, errNoSuchTask(id)
	}
	held := true
	defer func() {
		if held {
			resuming.Delete(id)
		}
	}()

	var rec spawnedRecord
	for _, e := range events {
		if e.Type == EvSubagentSpawned {
			_ = json.Unmarshal(e.Payload, &rec)
			break
		}
	}
	// A different role or model is a new task, not this one going on.
	if req.AgentType != "" && req.AgentType != rec.AgentType && req.AgentType != rec.Definition {
		return nil, fmt.Errorf("task %s ran as %s; resume it as that, or start a new task", id, orStr(rec.AgentType, rec.Definition))
	}
	if m := childModel(req.Model, ""); m != "" && m != rec.Provider {
		return nil, fmt.Errorf("task %s ran on model %s; resume it on that, or start a new task", id, orStr(rec.Provider, rec.Model))
	}
	// The role as it is now: its tools can never be wider than the session's today.
	def, found := f.Definitions.Get(orStr(rec.Definition, rec.AgentType))
	if !found {
		return nil, fmt.Errorf("task %s ran as agent type %s, which this session no longer offers; start a new task", id, orStr(rec.Definition, rec.AgentType))
	}
	// The organisation's pin on a managed role binds as it is now: a task
	// that ran on another model does not go on against it.
	if def.Source == SourceManaged && def.Model != "" && def.Model != rec.Provider {
		return nil, fmt.Errorf("agent type %s now runs on model %q, set by the organisation, and task %s ran on %s; start a new task",
			def.Name, def.Model, id, orStr(rec.Provider, rec.Model))
	}
	registry, err := childTools(f.Tools, def)
	if err != nil {
		return nil, err
	}
	adapter, provider, err := f.recordedModel(parent, rec)
	if err != nil {
		return nil, err
	}
	workspace, wt, err := f.recordedWorktree(ctx, rec)
	if err != nil {
		return nil, err
	}
	if err := f.reserve(rec.Depth, reserve); err != nil {
		return nil, err
	}

	msgs, err := Fork(events, 0)
	if err != nil {
		return nil, errNoSuchTask(id)
	}
	req.Description = orStr(req.Description, rec.Description)
	req.AgentType = rec.AgentType
	req.worktree = wt
	// A fresh allowance for this instruction, on top of the turns already spent.
	allowance := childTurns(f.Config.MaxTurns, def.MaxTurns, req.MaxTurns)
	c, spawned, err := f.build(parent, def, registry, adapter, provider, id, rec.Depth, req, workspace, end.Turns+allowance)
	if err != nil {
		return nil, err
	}
	// The window must hold what is rebuilt, with room to work: a subagent
	// has no compactor, by design.
	window := adapter.Profile().ContextWindow
	if used, err := adapter.CountTokens(model.Request{System: c.sub.Config.SystemPrompt, Messages: msgs}); err == nil && window > 0 && used*5 > window*4 {
		return nil, fmt.Errorf("task %s's conversation fills %d of %d tokens; start a new task with what it found", id, used, window)
	}
	c.sub.Recorder.Advance(events[len(events)-1].Seq)
	c.sub.SetHistory(msgs, end.Turns)
	c.before = len(msgs)
	c.sub.CarryUsage(end)

	spawned["resume"] = true
	spawned["through_seq"] = events[len(events)-1].Seq
	if rec.SHA256 != def.SHA256 {
		spawned["definition_changed"] = true
	}
	for k, v := range extra {
		spawned[k] = v
	}
	_, _ = c.sub.Recorder.Record(EvSubagentSpawned, ActorAgent, Trusted, spawned)
	c.parent.record(EvSubagentSpawned, ActorAgent, spawned)
	c.extra = extra
	c.release = func() { resuming.Delete(id) }
	if wt != nil {
		c.settle = settleLater(f.Workspace, wt)
	}
	held = false
	return c, nil
}

// recordedModel is the model the child ran on, and never another: the
// recorded provider, or the parent's own model when that is what it ran on.
func (f *SubagentFactory) recordedModel(parent *parentLink, rec spawnedRecord) (model.Adapter, string, error) {
	current := f.Adapter
	if parent != nil && parent.adapter != nil {
		current = parent.adapter
	}
	if rec.Provider != "" {
		if parent != nil && parent.provider == rec.Provider && current != nil {
			return current, rec.Provider, nil
		}
		if f.Models == nil {
			return nil, "", fmt.Errorf("the task ran on model %s, which is not available here; start a new task", rec.Provider)
		}
		a, err := f.Models(rec.Provider)
		if err != nil {
			return nil, "", fmt.Errorf("the task ran on model %s, which is not available: %w; start a new task", rec.Provider, err)
		}
		return a, rec.Provider, nil
	}
	if current != nil && current.Profile().Name == rec.Model {
		return current, "", nil
	}
	return nil, "", fmt.Errorf("the task ran on model %s, which is not available here; start a new task", rec.Model)
}

// recordedWorktree is the checkout a worktree child worked in, if it is still
// there, inside this workspace's worktrees and on the recorded branch. Only a
// child that worked in the workspace itself goes on there; one recorded
// anywhere else without a branch to verify is refused, never moved to the
// main tree.
func (f *SubagentFactory) recordedWorktree(ctx context.Context, rec spawnedRecord) (string, *worktree, error) {
	if rec.Workspace == "" || rec.Workspace == f.Workspace || realDir(rec.Workspace) == realDir(f.Workspace) {
		return f.Workspace, nil, nil
	}
	removed := errors.New("its worktree was removed; start a new task")
	if rec.Branch == "" {
		return "", nil, removed
	}
	root, err := hostgit.New(ctx, f.Workspace).Worktrees(ctx)
	if err != nil {
		return "", nil, removed
	}
	rel, err := filepath.Rel(root, rec.Workspace)
	if err != nil || !filepath.IsLocal(rel) || strings.Contains(rel, string(filepath.Separator)) {
		return "", nil, removed
	}
	if info, err := os.Lstat(rec.Workspace); err != nil || !info.IsDir() {
		return "", nil, removed
	}
	out, err := git(ctx, hostgit.New(ctx, f.Workspace), "worktree", "list", "--porcelain")
	if err != nil || !worktreeOnBranch(out, rec.Workspace, rec.Branch) {
		return "", nil, removed
	}
	return rec.Workspace, &worktree{Dir: rec.Workspace, Branch: rec.Branch, Start: rec.Start}, nil
}

// worktreeOnBranch reads `git worktree list --porcelain` for dir on branch.
func worktreeOnBranch(list, dir, branch string) bool {
	want := realDir(dir)
	for _, block := range strings.Split(list, "\n\n") {
		var path, head string
		for _, line := range strings.Split(block, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				path = p
			}
			if b, ok := strings.CutPrefix(line, "branch "); ok {
				head = b
			}
		}
		if path != "" && realDir(path) == want && head == "refs/heads/"+branch {
			return true
		}
	}
	return false
}

func realDir(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// validTaskID accepts what newID makes: letters and digits only.
func validTaskID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		digit, upper, lower := r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z'
		if !digit && !upper && !lower {
			return false
		}
	}
	return true
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
