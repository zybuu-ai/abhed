package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
			stat, n := worktreeChanges(ctx, o.worktree.Dir)
			if n == 0 {
				fmt.Fprintf(&b, "Worktree %s (branch %s): no changes. Remove with `git worktree remove %s`.\n\n",
					rel, o.worktree.Branch, rel)
			} else {
				fmt.Fprintf(&b, "Worktree %s (branch %s): %d file(s) changed, UNCOMMITTED.\n%s\n"+
					"To take these changes: review with `git -C %s diff`, commit there, then "+
					"`git merge %s` from the main tree. To discard: `git worktree remove --force %s && git branch -D %s`.\n\n",
					rel, o.worktree.Branch, n, stat, rel, o.worktree.Branch, rel, o.worktree.Branch)
			}
		}
	}
	if failed > 0 {
		fmt.Fprintf(&b, "[%d of %d tasks failed]\n", failed, len(outcomes))
	}
	return tools.Result{Content: b.String(), IsError: failed == len(outcomes)}
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

// WorktreeDir is where isolated checkouts live, inside the workspace so the
// parent's scoping boundary already covers them.
const WorktreeDir = ".abhed/worktrees"

func addWorktree(ctx context.Context, ws string) (*worktree, error) {
	id := newID()
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	dir := filepath.Join(ws, WorktreeDir, id)
	branch := "abhed/" + id
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	// Keep the worktrees out of `git status` for the main tree without
	// touching the repository's tracked .gitignore: info/exclude is local.
	r := hostgit.New(ctx, ws)
	excludeWorktrees(ctx, r)
	if _, err := git(ctx, r, "worktree", "add", "-b", branch, dir, "HEAD"); err != nil {
		return nil, err
	}
	return &worktree{Dir: dir, Branch: branch}, nil
}

func removeWorktree(ctx context.Context, ws string, wt *worktree) {
	r := hostgit.New(ctx, ws)
	_, _ = git(ctx, r, "worktree", "remove", "--force", wt.Dir)
	_, _ = git(ctx, r, "branch", "-D", wt.Branch)
}

func excludeWorktrees(ctx context.Context, r *hostgit.Repo) {
	ws := r.Dir
	gitDir, err := git(ctx, r, "rev-parse", "--git-common-dir")
	if err != nil {
		return
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(ws, gitDir)
	}
	p := filepath.Join(gitDir, "info", "exclude")
	// The git directory is the agent's to change, so the file is written as
	// the file tools write: under the workspace held open, never through a
	// link that leads out of it or into Abhed's state. A git directory
	// outside the workspace is left alone.
	c, err := tools.NewStateSet(ws).Confine(tools.RealPath(ws), ws)
	if err != nil {
		return
	}
	defer c.Close()
	if !c.Contains(p) {
		return
	}
	existing, err := c.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return
	}
	if bytes.Contains(existing, []byte(WorktreeDir)) {
		return
	}
	if err := c.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	_ = c.WriteAtomic(p, append(existing, []byte("\n# Abhed subagent worktrees\n"+WorktreeDir+"/\n")...), 0o644)
}

// worktreeChanges summarises what a subagent left behind: a diff stat and
// the number of changed paths, counting untracked files as changes too.
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
