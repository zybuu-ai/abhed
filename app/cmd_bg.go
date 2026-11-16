package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/bgcli"
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
// every folder in the repository with a .abhed of its own. Links are not
// followed, and .git is not entered.
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
	err = filepath.WalkDir(realRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable folder holds no rules we can read
		}
		if !d.IsDir() {
			return nil
		}
		switch d.Name() {
		case ".git":
			return filepath.SkipDir
		case ".abhed":
			add(filepath.Dir(p))
			return filepath.SkipDir
		}
		return nil
	})
	return dirs, err
}
