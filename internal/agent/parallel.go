package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Parallel subagents, optionally each in its own git worktree.
//
// The single `task` tool runs one subagent and waits. This runs several at
// once — the shape of "investigate these three modules" or "try both fixes and
// tell me which passes". The interesting case is when they WRITE: two agents
// editing one checkout will overwrite each other, and the parent cannot tell
// whose change survived. Isolation gives each child its own worktree on its
// own branch; the parent gets back what changed and how to merge it, and
// nothing touches the main tree until a person decides it should.

// Tasks is the tool.
type Tasks struct {
	Spawn    func(ctx context.Context, req SubagentRequest) (string, error)
	Profiles map[string]PromptProfile
	// Workspace is the parent's root; worktrees are created beneath it.
	Workspace string
	// MaxParallel bounds concurrency. Zero means all at once.
	MaxParallel int
}

func (Tasks) Name() string  { return "tasks" }
func (Tasks) Mutates() bool { return false } // each child's tools are policed on their own

func (t Tasks) Description() string {
	return "Run several subagents AT THE SAME TIME, each with a fresh context and a " +
		"complete self-contained prompt. Use for independent subtasks — investigating " +
		"separate areas, or trying alternative approaches. Set isolation to \"worktree\" " +
		"when subagents will edit files: each then works on its own git branch in its " +
		"own checkout, and you receive each branch's changes and how to merge them. " +
		"Returns every subagent's summary, in order."
}

func (Tasks) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "tasks":{
      "type":"array","minItems":1,"maxItems":8,
      "items":{
        "type":"object",
        "properties":{
          "prompt":{"type":"string","description":"Complete, self-contained task. The subagent sees none of this conversation."},
          "description":{"type":"string","description":"3-5 word label shown to the user."},
          "agent_type":{"type":"string","description":"explore | test | review | general. Defaults to general."},
          "max_turns":{"type":"integer"}
        },
        "required":["prompt","description"]
      }
    },
    "isolation":{"type":"string","enum":["none","worktree"],"description":"worktree: each subagent edits its own git branch in its own checkout. Required when subagents will change files. Default none."}
  },
  "required":["tasks"]
}`)
}

type tasksArgs struct {
	Tasks     []taskArgs `json:"tasks"`
	Isolation string     `json:"isolation"`
}

type taskOutcome struct {
	index    int
	desc     string
	summary  string
	err      error
	worktree *worktree
}

type worktree struct {
	Dir    string
	Branch string
	Start  string // the commit it was made at
}

func (t Tasks) Run(ctx context.Context, _ *tools.Session, raw json.RawMessage) tools.Result {
	var a tasksArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return tools.Result{Content: fmt.Sprintf("Invalid arguments for tasks: %v", err), IsError: true}
	}
	if len(a.Tasks) == 0 {
		return tools.Result{Content: "tasks must name at least one task.", IsError: true}
	}
	if len(a.Tasks) > 8 {
		return tools.Result{Content: "tasks is limited to 8 subagents at once.", IsError: true}
	}
	for i, tk := range a.Tasks {
		if strings.TrimSpace(tk.Prompt) == "" {
			return tools.Result{Content: fmt.Sprintf("tasks[%d].prompt is required and must be self-contained.", i), IsError: true}
		}
	}

	isolate := a.Isolation == "worktree"
	if isolate {
		if err := requireGitRepo(ctx, t.Workspace); err != nil {
			return tools.Result{Content: "isolation \"worktree\" needs the workspace to be a git " +
				"repository: " + err.Error() + ". Use isolation \"none\", or initialise git first.",
				IsError: true}
		}
	}

	// Worktrees are created up front and serially: git's own locking makes
	// concurrent `worktree add` fragile, and a failure here should stop the
	// whole call before any subagent has spent a token.
	var trees []*worktree
	if isolate {
		for range a.Tasks {
			wt, err := addWorktree(ctx, t.Workspace)
			if err != nil {
				for _, done := range trees {
					removeWorktree(context.Background(), t.Workspace, done)
				}
				return tools.Result{Content: "could not create a worktree: " + err.Error(), IsError: true}
			}
			trees = append(trees, wt)
		}
	}

	limit := t.MaxParallel
	if limit <= 0 || limit > len(a.Tasks) {
		limit = len(a.Tasks)
	}
	sem := make(chan struct{}, limit)
	outcomes := make([]taskOutcome, len(a.Tasks))
	var wg sync.WaitGroup
	for i, tk := range a.Tasks {
		wg.Add(1)
		go func(i int, tk taskArgs) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req := SubagentRequest{Prompt: tk.Prompt, Description: tk.Description,
				AgentType: tk.AgentType, MaxTurns: tk.MaxTurns}
			var wt *worktree
			if isolate {
				wt = trees[i]
				req.Workspace = wt.Dir
			}
			summary, err := t.Spawn(ctx, req)
			outcomes[i] = taskOutcome{index: i, desc: tk.Description, summary: summary, err: err, worktree: wt}
		}(i, tk)
	}
	wg.Wait()

	var b strings.Builder
	failed := 0
	for _, o := range outcomes {
		fmt.Fprintf(&b, "## Task %d — %s\n", o.index+1, o.desc)
		if o.err != nil {
			failed++
			fmt.Fprintf(&b, "FAILED: %v\n\n", o.err)
		} else {
			b.WriteString(strings.TrimSpace(o.summary))
			b.WriteString("\n\n")
		}
		if o.worktree != nil {
			rel, _ := filepath.Rel(t.Workspace, o.worktree.Dir)
			b.WriteString(t.settle(ctx, rel, o.worktree))
		}
	}
	if failed > 0 {
		fmt.Fprintf(&b, "[%d of %d tasks failed]\n", failed, len(outcomes))
	}
	return tools.Result{Content: b.String(), IsError: failed == len(outcomes)}
}

// settle removes a worktree the subagent left as it was made, with its branch,
// and otherwise keeps it and says what it holds and how to take it.
func (t Tasks) settle(ctx context.Context, rel string, wt *worktree) string {
	untouched, err := hostgit.Untouched(ctx, wt.Dir, wt.Start)
	if err != nil {
		return fmt.Sprintf("Worktree %s (branch %s): its state could not be read (%v); it is kept.\n\n", rel, wt.Branch, err)
	}
	if untouched {
		removeWorktree(context.WithoutCancel(ctx), t.Workspace, wt)
		return fmt.Sprintf("Worktree %s (branch %s): no changes; removed with its branch.\n\n", rel, wt.Branch)
	}
	discard := fmt.Sprintf("To discard: `git worktree remove --force %s && git branch -D %s`.", rel, wt.Branch)
	if stat, n := worktreeChanges(ctx, wt.Dir); n > 0 {
		return fmt.Sprintf("Worktree %s (branch %s): %d file(s) changed, UNCOMMITTED.\n%s\n"+
			"To take these changes: review with `git -C %s diff`, commit there, then "+
			"`git merge %s` from the main tree. %s\n\n", rel, wt.Branch, n, stat, rel, wt.Branch, discard)
	}
	return fmt.Sprintf("Worktree %s (branch %s): nothing uncommitted, but it holds commits or ignored files, "+
		"so it is kept. Review with `git log %s..%s`, then `git merge %s` from the main tree. %s\n\n",
		rel, wt.Branch, wt.Start[:min(12, len(wt.Start))], wt.Branch, wt.Branch, discard)
}

// ------------------------------------------------------------------ git

// git runs one command on r, whose drivers were read once for the operation.
func git(ctx context.Context, r *hostgit.Repo, args ...string) (string, error) {
	cmd := r.Command(ctx, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

func requireGitRepo(ctx context.Context, ws string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git is not installed")
	}
	_, err := git(ctx, hostgit.New(ctx, ws), "rev-parse", "--git-dir")
	return err
}

// WorktreeDir is where isolated checkouts live: inside the workspace, and outside
// Abhed's state so the subagents can write there.
const WorktreeDir = tools.WorktreesDir

func addWorktree(ctx context.Context, ws string) (*worktree, error) {
	id := newID()
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	r := hostgit.New(ctx, ws)
	parent, err := r.Worktrees(ctx)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(parent, id)
	if err := hostgit.NewWorktreeDir(dir); err != nil {
		return nil, err
	}
	branch := "abhed/" + id
	if _, err := git(ctx, r, "worktree", "add", "-b", branch, dir, "HEAD"); err != nil {
		return nil, err
	}
	wt := &worktree{Dir: dir, Branch: branch}
	if err := r.Placed(ctx, dir); err != nil {
		_, _ = git(ctx, r, "branch", "-D", branch)
		return nil, err
	}
	start, err := hostgit.Head(ctx, dir)
	if err != nil {
		removeWorktree(ctx, ws, wt)
		return nil, err
	}
	wt.Start = start
	return wt, nil
}

func removeWorktree(ctx context.Context, ws string, wt *worktree) {
	r := hostgit.New(ctx, ws)
	_, _ = git(ctx, r, "worktree", "remove", "--force", wt.Dir)
	_, _ = git(ctx, r, "branch", "-D", wt.Branch)
	hostgit.RemoveWorktrees(ws)
}

// worktreeChanges summarises what a subagent left uncommitted: a diff stat
// and the number of changed paths, untracked included.
func worktreeChanges(ctx context.Context, dir string) (string, int) {
	r := hostgit.New(ctx, dir)
	status, err := git(ctx, r, "status", "--porcelain")
	if err != nil || status == "" {
		return "", 0
	}
	n := len(strings.Split(status, "\n"))
	stat, _ := git(ctx, r, "diff", "--stat", "HEAD")
	if stat == "" {
		stat = status
	}
	return stat, n
}
