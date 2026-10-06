// Package hostgit runs the git commands Abhed itself runs on the host, for
// its own bookkeeping: the branch and dirty count in the prompt, worktrees for
// parallel runs, and a resolved issue's commit and push.
//
// These run outside the sandbox, in a repository the agent can write. A
// repository's own configuration can name programs git runs (an fsmonitor, a
// hook, a clean or smudge filter, a textconv, an external diff, a signing
// program, a transport), so a planted .git/config would run them here with
// the server's privileges. The settings known to do that for the commands
// Abhed runs are switched off in git's command-line scope, which wins over
// every configuration file, submodules are not entered, and git's own
// environment is dropped. The common git folder, where configuration, hooks
// and refs are read from, is found from where the git folder is and named to
// git outright, and a command is refused while a commondir file in the git
// folder points anywhere else: a sandboxed command could have planted one.
//
// This is a list, and so best effort: a git release that adds a setting
// naming a program is not covered until it is added here. Running these
// commands inside the sandbox is the complete answer, and is tracked apart.
package hostgit

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// safety switches off every setting known to name a program git would run
// for the commands Abhed uses.
var safety = [][2]string{
	{"core.fsmonitor", "false"},
	{"core.hooksPath", "/dev/null"},
	{"core.sshCommand", "ssh"},
	{"core.askPass", ""},
	{"core.gitProxy", ""},
	{"credential.helper", ""},
	{"diff.external", ""},
	{"commit.gpgSign", "false"},
	{"tag.gpgSign", "false"},
	{"push.gpgSign", "false"},
	{"submodule.recurse", "false"},
	{"status.submoduleSummary", "false"},
	{"diff.ignoreSubmodules", "all"},
	// Only HTTPS moves data; GIT_ALLOW_PROTOCOL in Env enforces it over any
	// per-protocol setting, and these keep the default the same.
	{"protocol.allow", "never"},
	{"protocol.https.allow", "always"},
}

// Repo runs git on one repository. The drivers its configuration names are
// read once, when it is made, for all the commands of one operation.
type Repo struct {
	Dir     string
	drivers [][2]string
	// gitDir is dir's git folder, and common the common one, found from
	// where the git folder is rather than from a commondir file in it;
	// both "" outside a repository.
	gitDir, common string
	// unknown is why git is not run: a .git is there, but git could not
	// say where its git folder is, so nothing can be pinned.
	unknown error
}

// New finds dir's git folders and reads the configuration for the filter
// and merge drivers it names. Neither runs anything.
func New(ctx context.Context, dir string) *Repo {
	gitDir, common, err := gitDirs(ctx, dir)
	if err != nil {
		return &Repo{Dir: dir, unknown: err}
	}
	return &Repo{Dir: dir, gitDir: gitDir, common: common, drivers: drivers(ctx, dir, common)}
}

// Command builds a git command on the repository with its program-running
// settings off.
func (r *Repo) Command(ctx context.Context, args ...string) *exec.Cmd {
	return r.CommandWith(ctx, nil, nil, args...)
}

// CommandWith is Command with more configuration, set in the same
// command-line scope, and more environment, added after git's own is dropped.
func (r *Repo) CommandWith(ctx context.Context, config [][2]string, env []string, args ...string) *exec.Cmd {
	if len(args) > 0 {
		switch args[0] {
		case "diff", "show", "log":
			// A diff driver's command or textconv is switched off by flag.
			args = append([]string{args[0], "--no-ext-diff", "--no-textconv", "--ignore-submodules=all"}, args[1:]...)
		case "status":
			args = append([]string{args[0], "--ignore-submodules=all"}, args[1:]...)
		}
	}
	all := append(append(append([][2]string(nil), safety...), r.drivers...), config...)
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Dir}, args...)...) // #nosec G204 -- fixed binary, arguments from Abhed
	place(cmd, r.Dir)
	if r.unknown != nil && cmd.Err == nil {
		cmd.Err = r.unknown
	}
	if err := r.redirected(); err != nil && cmd.Err == nil {
		cmd.Err = err
	}
	cmd.Env = append(append(r.env(env), configEnv(all)...), env...)
	return cmd
}

// env is Env with the common git folder named, unless more names the git
// folder itself, as for a bare repository Abhed makes.
func (r *Repo) env(more []string) []string {
	return pinned(r.common, more)
}

func pinned(common string, more []string) []string {
	out := Env()
	if common == "" {
		return out
	}
	for _, kv := range more {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_COMMON_DIR=") {
			return out
		}
	}
	return append(out, "GIT_COMMON_DIR="+common)
}

// gitDirs are dir's git folder and its common git folder: the same one, or
// for a linked worktree's (<common>/worktrees/<name>) the folder two up. A
// commondir file is not read, since a sandboxed command can write one; ""
// for both when dir is in no repository. When a .git is at or above dir but
// git cannot name the git folder, the error is why no git is run there:
// unpinned, git would follow whatever commondir it found.
func gitDirs(ctx context.Context, dir string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--absolute-git-dir") // #nosec G204 -- fixed arguments
	place(cmd, dir)
	cmd.Env = Env()
	out, err := cmd.Output()
	gd := filepath.Clean(strings.TrimSpace(string(out)))
	if err != nil || !filepath.IsAbs(gd) {
		if root := repoRoot(dir); root != "" {
			var ee *exec.ExitError
			switch {
			case err == nil:
				err = fmt.Errorf("git named %q", gd)
			case errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0:
				err = errors.New(string(bytes.TrimSpace(ee.Stderr)))
			}
			return "", "", fmt.Errorf("git could not say where the git folder of %s is (%v), "+
				"so it cannot be pinned against a commondir planted there; Abhed runs no git in this repository", root, err)
		}
		return "", "", nil
	}
	if parent := filepath.Dir(gd); strings.EqualFold(filepath.Base(parent), "worktrees") {
		if c := filepath.Dir(parent); isGitDir(c) {
			return gd, c, nil
		}
	}
	return gd, gd, nil
}

// redirected says why git is not run while a commondir file in the git
// folder points anywhere but the common git folder found. Git reads refs
// through that file whatever GIT_COMMON_DIR says, so naming the folder is
// not enough.
func (r *Repo) redirected() error {
	if r.gitDir == "" {
		return nil
	}
	p := filepath.Join(r.gitDir, "commondir")
	data, err := os.ReadFile(p) // #nosec G304 -- the repository's own git folder
	if errors.Is(err, fs.ErrNotExist) {
		if r.gitDir == r.common {
			return nil
		}
		return fmt.Errorf("%s is missing, so git cannot be told which repository the worktree belongs to; Abhed runs no git there", p)
	}
	if err == nil && r.gitDir != r.common {
		to := strings.TrimRight(string(data), "\r\n")
		if !filepath.IsAbs(to) {
			to = filepath.Join(r.gitDir, to)
		}
		if sameDir(to, r.common) {
			return nil
		}
	}
	return fmt.Errorf("%s points git at another folder's configuration, hooks and refs, and git never writes one there; "+
		"a command may have planted it. Abhed runs no git in this repository until it is removed", p)
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// isGitDir reports whether d holds what every git folder does.
func isGitDir(d string) bool {
	head, err := os.Lstat(filepath.Join(d, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	objects, err := os.Stat(filepath.Join(d, "objects"))
	return err == nil && objects.IsDir()
}

// GitPath is the git that runs for dir: the one on PATH, with its links
// followed. One inside dir, or in a folder sandboxed commands may write, is
// refused: the agent could have put it there, and it would run on the host.
func GitPath(dir string) (string, error) {
	p, err := exec.LookPath("git")
	if err != nil {
		return "", err
	}
	real := sandbox.RealPath(p)
	areas := append([]string{dir, repoRoot(dir)}, sandbox.WritableAreas()...)
	for _, area := range areas {
		if area == "" {
			continue
		}
		for _, a := range sandbox.PathForms(area) {
			if _, in := sandbox.Within(real, a); in {
				return "", fmt.Errorf("the git on PATH (%s) is inside %s, which the agent's commands can write; "+
					"Abhed runs no git from there. Put a system git first on PATH", real, a)
			}
		}
	}
	return p, nil
}

// repoRoot is the nearest folder at or above dir that holds a .git, or "".
func repoRoot(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// place points cmd at GitPath(dir), or makes it fail to start with why.
func place(cmd *exec.Cmd, dir string) {
	p, err := GitPath(dir)
	if err != nil {
		cmd.Err = err
		return
	}
	cmd.Path = p
}

// Command is New(ctx, dir).Command, for a single command.
func Command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	return New(ctx, dir).Command(ctx, args...)
}

// configEnv sets configuration in git's command-line scope through its
// environment, which, unlike -c, takes any name: a driver called a=b too.
func configEnv(kv [][2]string) []string {
	out := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(kv))}
	for i, p := range kv {
		n := strconv.Itoa(i)
		out = append(out, "GIT_CONFIG_KEY_"+n+"="+p[0], "GIT_CONFIG_VALUE_"+n+"="+p[1])
	}
	return out
}

// Env is the host environment without git's own variables, which could point
// git at another repository or configuration, or name programs it runs.
func Env() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	// Only https, whatever protocol.<name>.allow a configuration sets: a
	// transport such as ext:: runs a program, with the push token in view.
	return append(out, "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=https")
}

// drivers clears the command of every filter and merge driver the
// repository's configuration names.
func drivers(ctx context.Context, dir, common string) [][2]string {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "config", "--includes", "--name-only", "-z", "--get-regexp", `^(filter|merge)\.`) // #nosec G204 -- fixed arguments
	place(cmd, dir)
	cmd.Env = pinned(common, nil)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Split(splitNUL)
	for sc.Scan() {
		key := sc.Text()
		section, rest, ok := strings.Cut(key, ".")
		if !ok {
			continue
		}
		i := strings.LastIndex(rest, ".")
		if i <= 0 {
			continue
		}
		seen[strings.ToLower(section)+"."+rest[:i]] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	var kv [][2]string
	for _, n := range names {
		switch {
		case strings.HasPrefix(n, "filter."):
			kv = append(kv, [2]string{n + ".clean", ""}, [2]string{n + ".smudge", ""},
				[2]string{n + ".process", ""}, [2]string{n + ".required", "false"})
		case strings.HasPrefix(n, "merge."):
			kv = append(kv, [2]string{n + ".driver", ""})
		}
	}
	return kv
}

func splitNUL(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
