// Package sandboxconfig builds the sandbox a configuration asks for, so the
// command line and the SDK cannot read sandbox settings differently.
package sandboxconfig

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/nlink"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Build selects an execution backend meeting the configured minimum tier; Select
// never downgrades. stateRoots' .abhed is state too, as a worktree's repository's is.
func Build(cfg config.Config, workspace string, stateRoots ...string) (sandbox.Sandbox, error) {
	if err := CheckStatePaths(cfg, workspace, stateRoots...); err != nil {
		return nil, err
	}
	p := sandbox.DefaultPolicy(workspace)
	if cfg.Sandbox.MinTier != "" {
		p.MinTier = sandbox.Tier(cfg.Sandbox.MinTier)
	}
	p.AllowNetwork = cfg.Sandbox.AllowNetwork
	p.ReadOnlyPaths = cfg.Sandbox.ReadOnlyPaths
	p.StatePaths = StatePaths(cfg, workspace)
	for _, r := range stateRoots {
		if filepath.Clean(r) != filepath.Clean(workspace) {
			p.StatePaths = append(p.StatePaths, filepath.Join(r, tools.StateDir))
		}
	}
	if cfg.Sandbox.MaxMemoryMB > 0 {
		p.MaxMemoryMB = cfg.Sandbox.MaxMemoryMB
	}
	if cfg.Sandbox.MaxProcs > 0 {
		p.MaxProcs = cfg.Sandbox.MaxProcs
	}
	return sandbox.Select(p)
}

// StatePaths are the files holding Abhed's state that a configuration can put
// outside .abhed: the local accounts and the secrets store. The tools, the
// server and the sandbox all refuse them.
func StatePaths(cfg config.Config, workspace string) []string {
	users := cfg.Auth.UsersFile
	if users == "" {
		users = filepath.Join(workspace, ".abhed", "users.json")
	}
	out := []string{users}
	if path, err := secrets.DefaultPath(); err == nil {
		out = append(out, path)
	}
	return out
}

// CheckStatePaths refuses a configured state file that commands could reach:
// one inside the workspace, an added directory, a temp folder or a toolchain
// cache, unless it is under the workspace's or the home directory's .abhed,
// which the sandbox denies whole. A deny rule on the file alone does not hold
// there, since a command can rename a folder above it or plant a link where
// it will be created. A path that does not exist yet is judged by its deepest
// existing parent, with links followed.
func CheckStatePaths(cfg config.Config, workspace string, stateRoots ...string) error {
	// A .abhed is a shield only where it really is: not a link to a folder
	// elsewhere, which the sandbox's rule for it would not cover.
	var shielded []string
	bases := []string{workspace}
	if home, err := os.UserHomeDir(); err == nil {
		bases = append(bases, home)
	}
	for _, b := range bases {
		abs, err := filepath.Abs(b)
		if err != nil {
			continue
		}
		want := filepath.Join(tools.RealPath(abs), ".abhed")
		if tools.RealPath(filepath.Join(abs, ".abhed")) == want {
			shielded = append(shielded, want)
		}
	}
	areas := append([]string{workspace}, cfg.AdditionalDirs...)
	areas = append(areas, sandbox.WritableAreas()...)
	for _, p := range StatePaths(cfg, workspace) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		real := tools.RealPath(abs)
		if inside(real, shielded) {
			continue
		}
		for _, a := range areas {
			if under([]string{abs, real}, []string{a}) {
				return fmt.Errorf("refusing to start: the state file %s is inside %s, where commands "+
					"can write and could move it or replace it. Keep auth.users_file and "+
					"ABHED_SECRETS_FILE outside the workspace, added directories, temp folders and "+
					"caches, or under the workspace's or the home directory's .abhed", p, a)
			}
		}
	}
	return checkLinks(cfg, workspace, stateRoots)
}

// checkLinks refuses a state file with a second name: the sandbox guards the
// state by path, so a command could rewrite the file through the other name.
func checkLinks(cfg config.Config, workspace string, stateRoots []string) error {
	roots := append(append([]string{workspace}, cfg.AdditionalDirs...), stateRoots...)
	linked := tools.NewStateSet(roots...).Linked()
	linked = append(linked, StatePaths(cfg, workspace)...)
	for _, p := range linked {
		n, err := nlink.Linked(p)
		if err != nil {
			return fmt.Errorf("refusing to start: the state file %s cannot be checked: %w", p, err)
		}
		if n > 0 {
			return fmt.Errorf("refusing to start: %w", nlink.Refusal(p, n))
		}
	}
	return nil
}

// inside reports whether a resolved path lies in one of the resolved dirs.
func inside(real string, dirs []string) bool {
	for _, d := range dirs {
		if rel, err := filepath.Rel(d, real); err == nil && filepath.IsLocal(rel) {
			return true
		}
	}
	return false
}

// under reports whether any spelling of a path lies in any of dirs, each
// dir also compared with its links followed.
func under(spellings, dirs []string) bool {
	for _, d := range dirs {
		if d == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		for _, dir := range []string{abs, tools.RealPath(abs)} {
			for _, s := range spellings {
				rel, err := filepath.Rel(dir, s)
				if err == nil && filepath.IsLocal(rel) {
					return true
				}
			}
		}
	}
	return false
}
