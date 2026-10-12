package hostgit

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// Broker wraps a sandbox's Command so the few git commands the sandbox keeps
// from working (it holds git's worktree and submodule folders) run through
// host git instead, with this package's settings and checks: git worktree
// add, remove and prune, and git submodule update. Each must be the whole
// command line, in plain words, with every path it names inside the
// workspace. Anything else goes to command as it was. Only the process tier
// and the fence are brokered, as only they run commands on this host.
func Broker(workspace string, tier func() sandbox.Tier, command func(ctx context.Context, cwd, line string) *exec.Cmd) func(ctx context.Context, cwd, line string) *exec.Cmd {
	return func(ctx context.Context, cwd, line string) *exec.Cmd {
		args, ok := brokered(line)
		if !ok || tier == nil {
			return command(ctx, cwd, line)
		}
		switch tier() {
		case sandbox.TierProcess, sandbox.TierFence:
		default:
			return command(ctx, cwd, line)
		}
		if err := brokerPaths(workspace, cwd, args); err != nil {
			return &exec.Cmd{Err: err}
		}
		// On the host, whatever sandbox is registered for the workspace.
		return New(ctx, cwd).commandWith(ctx, nil, nil, true, args...)
	}
}

// brokered is the git arguments of a line that is one of the brokered
// commands, in plain words: no quoting, expansion or second command.
func brokered(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	for _, r := range line {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(" ./_-+@:=", r)) {
			return nil, false
		}
	}
	f := strings.Fields(line)
	if len(f) < 3 || f[0] != "git" {
		return nil, false
	}
	args := f[1:]
	switch args[0] + " " + args[1] {
	case "worktree add", "worktree remove", "worktree prune", "submodule update":
	default:
		return nil, false
	}
	for _, a := range args[2:] {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		flag, _, _ := strings.Cut(a, "=")
		if !brokerFlags[args[0]+" "+args[1]][flag] {
			return nil, false
		}
	}
	return args, true
}

// brokerFlags are the options each brokered command takes; any other is
// left to the sandbox, which may refuse it.
var brokerFlags = map[string]map[string]bool{
	"worktree add":     {"-b": true, "-B": true, "--detach": true, "-f": true, "--force": true, "-q": true, "--quiet": true, "--no-checkout": true, "--checkout": true},
	"worktree remove":  {"-f": true, "--force": true},
	"worktree prune":   {"-n": true, "--dry-run": true, "-v": true, "--verbose": true},
	"submodule update": {"--init": true, "--recursive": true, "-q": true, "--quiet": true, "--checkout": true, "--no-fetch": true, "-N": true},
}

// brokerPaths refuses a brokered command naming a path outside the
// workspace, so host git writes only there.
func brokerPaths(workspace, cwd string, args []string) error {
	ws := sandbox.PathForms(workspace)
	in := func(p string) bool {
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		p = filepath.Clean(p)
		for _, w := range ws {
			if _, ok := sandbox.Within(p, w); ok {
				if _, ok := sandbox.Within(sandbox.RealPath(p), sandbox.RealPath(w)); ok {
					return true
				}
			}
		}
		return false
	}
	if !in(cwd) {
		return errors.New("git: the command runs outside the workspace, so Abhed does not run it on the host")
	}
	if args[0] != "worktree" || args[1] == "prune" {
		return nil
	}
	// The first word that is not an option or an option's value is the path.
	for i := 2; i < len(args); i++ {
		a := args[i]
		if a == "-b" || a == "-B" {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if !in(a) {
			return fmt.Errorf("git worktree %s: %s is outside the workspace, so Abhed does not run it on the host", args[1], a)
		}
		return nil
	}
	return nil
}
