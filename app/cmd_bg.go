package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/bgcli"
	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// bgCmd is `abhed bg`, background jobs on a server, as the profile abhed
// login saved.
func bgCmd(workspace string, args []string, trust config.TrustChoice, in io.Reader, out, errOut io.Writer) int {
	if !profileNetwork(errOut) {
		return bgcli.ExitFailed
	}
	path, err := profilesPath(workspace)
	if err != nil {
		_, _ = io.WriteString(errOut, "abhed: "+err.Error()+"\n")
		return bgcli.ExitFailed
	}
	// Ctrl-C leaves attach; the job runs on.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env := bgcli.Env{Ctx: ctx, ProfilesPath: path, In: in, Out: out, Err: errOut, InAgent: inAgentCommand,
		Workspace: workspace}
	env.RulesFor = bgRulesFor(trust, workspace)
	// Nor a value from the person's secrets store.
	if red, err := secrets.Default().LoadRedactor(); err != nil {
		env.SnapshotErr = err
	} else {
		env.HoldsStored = func(b []byte) bool { return !bytes.Equal(red.Redact(b), b) }
	}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) && term.IsTerminal(int(os.Stderr.Fd())) {
		env.Interactive = true
	}
	return bgcli.Main(env, args)
}

// bgRulesFor is the person's read rules for a whole repository: a file is
// withheld when any configuration a session in the repository would load
// withholds it. That is the root's, the one of the folder bg runs in
// (workspace) and of every folder between, and of every other folder in the
// repository with a .abhed of its own, since a session opened there would
// honour it. Each is loaded as a session there would load it, with the trust
// its own folder has.
func bgRulesFor(trust config.TrustChoice, workspace string) func(root string) (func(string) bool, error) {
	return func(root string) (func(string) bool, error) {
		dirs, err := bgRuleFolders(root, workspace)
		if err != nil {
			return nil, err
		}
		var sets []func(string) bool
		for _, dir := range dirs {
			cfg, err := config.LoadWith(dir, config.LoadOptions{Quiet: true, Trust: config.GrantFor(trust, workspace, dir)})
			if err != nil {
				return nil, fmt.Errorf("the configuration at %s, which says what a snapshot may carry: %w", dir, err)
			}
			sets = append(sets, toolset.ReadWithheld(cfg, dir))
		}
		return func(abs string) bool {
			for _, withheld := range sets {
				if withheld(abs) {
					return true
				}
			}
			return false
		}, nil
	}
}

// bgRuleFolders are the folders whose configuration a snapshot of the
// repository at root follows: root, those from it down to workspace, and
// every folder holding a tracked file that has a .abhed of its own. Only
// folders with tracked files are looked in, so an untracked tree such as
// node_modules is never walked; a snapshot carries tracked files alone.
func bgRuleFolders(root, workspace string) ([]string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var dirs []string
	add := func(d string) {
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	add(realRoot)
	if ws, err := filepath.EvalSymlinks(workspace); err == nil {
		if rel, err := filepath.Rel(realRoot, ws); err == nil && filepath.IsLocal(rel) {
			d := realRoot
			for _, part := range strings.Split(rel, string(filepath.Separator)) {
				d = filepath.Join(d, part)
				add(d)
			}
		}
	}
	out, err := hostgit.Command(context.Background(), realRoot, "ls-files", "-z", "--cached").Output()
	if err != nil {
		return nil, fmt.Errorf("list the repository's files: %w", err)
	}
	looked := map[string]bool{}
	for _, name := range strings.Split(string(out), "\x00") {
		for d := path.Dir(name); name != "" && !looked[d]; d = path.Dir(d) {
			looked[d] = true
			dir := filepath.Join(realRoot, filepath.FromSlash(d))
			if fi, err := os.Lstat(filepath.Join(dir, ".abhed")); err == nil && fi.IsDir() {
				add(dir)
			}
			if d == "." {
				break
			}
		}
	}
	return dirs, nil
}
