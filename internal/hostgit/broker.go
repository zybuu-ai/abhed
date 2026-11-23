package hostgit

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Brokered reports whether line is a git command run on the host for the
// sandbox, or refused there, which a wrapper must pass on as it is.
func Brokered(line string) bool {
	if _, _, ok := parseRemote(line); ok {
		return true
	}
	_, ok := brokered(line)
	return ok
}

// BrokersRemotes reports whether git's remote operations (fetch, pull, push,
// clone) are run on the host for sb, as the broker decides: on the process
// tier and the fence, where sb holds git's configuration against commands.
func BrokersRemotes(sb sandbox.Sandbox) bool {
	if sb == nil || sb.Tier() != sandbox.TierProcess && sb.Tier() != sandbox.TierFence || !sandbox.HoldsGit(sb) {
		return false
	}
	for _, l := range []string{"git fetch origin", "git pull origin main", "git push origin main", "git clone https://example.com/r.git"} {
		if Brokered(l) {
			return true
		}
	}
	return false
}

func init() { tools.HostGitLine = Brokered }

// noProtocol names no transport git has, so git reaches no remote.
const noProtocol = "abhed-none"

// brokered is the git arguments of a line that is one of the brokered
// commands, in plain words: no quoting, expansion or second command.
func brokered(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	for _, r := range line {
		plain := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(" ./_-+@:=", r)
		if !plain {
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
