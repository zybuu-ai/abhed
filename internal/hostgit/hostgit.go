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
// environment is dropped. The common git folder is named to git outright,
// and git is refused while a commondir, which a command could plant, is not git's own.
//
// This is a list, and so best effort: a git release that adds a setting
// naming a program is not covered until it is added here. So a surface that
// has a sandbox registers it with Register, and every command on the
// workspace's repository then runs inside it as well, the list still applied;
// only worktree add and remove, which the sandbox keeps from writing git's
// worktree folders, and commands on a repository Abhed made itself (the bare
// one a push goes from) stay on the host.
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
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

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

// confinement is a sandbox registered for the repositories under root.
type confinement struct {
	roots []string
	sb    sandbox.Sandbox
}

var (
	confineMu sync.Mutex
	confines  []*confinement
)

// Register runs git commands on the repositories under workspace inside sb
// from now on, when sb runs commands on this host (the process tier or the
// fence); the container, vm and none tiers leave them on the host. It
// returns what undoes it; the latest registration for a folder wins.
func Register(workspace string, sb sandbox.Sandbox) (unregister func()) {
	if sb == nil || workspace == "" {
		return func() {}
	}
	c := &confinement{roots: sandbox.PathForms(workspace), sb: sb}
	confineMu.Lock()
	confines = append(confines, c)
	confineMu.Unlock()
	return func() {
		confineMu.Lock()
		defer confineMu.Unlock()
		confines = slices.DeleteFunc(confines, func(x *confinement) bool { return x == c })
	}
}

// confinedBy is the sandbox registered for dir that runs commands on this
// host, or nil.
func confinedBy(dir string) sandbox.Sandbox {
	forms := sandbox.PathForms(dir)
	confineMu.Lock()
	var found *confinement
	for i := len(confines) - 1; i >= 0 && found == nil; i-- {
		for _, r := range confines[i].roots {
			for _, f := range forms {
				if _, in := sandbox.Within(f, r); in {
					found = confines[i]
				}
			}
		}
	}
	confineMu.Unlock()
	if found == nil {
		return nil
	}
	switch found.sb.Tier() {
	case sandbox.TierProcess, sandbox.TierFence:
		return found.sb
	}
	return nil
}

// Confined reports whether git commands in dir run inside a sandbox.
func Confined(dir string) bool { return confinedBy(dir) != nil }

// onHost reports whether a command stays on the host: worktree add and
// remove make and remove folders under the git folder the sandbox holds, and
// a command naming its git folder outright is on a repository Abhed made.
func onHost(args, env []string) bool {
	if len(args) > 0 && args[0] == "worktree" && len(args) > 1 && args[1] != "list" {
		return true
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_DIR=") {
			return true
		}
	}
	return false
}

// gitCmd builds git in dir with env added to git's own dropped: inside the
// registered sandbox unless host is set or none is registered.
func gitCmd(ctx context.Context, dir string, host bool, env []string, args ...string) *exec.Cmd {
	sb := confinedBy(dir)
	if sb == nil || host {
		cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed binary, arguments from Abhed
		place(cmd, dir)
		cmd.Env = env
		return cmd
	}
	p, err := GitPath(dir)
	if err != nil {
		return &exec.Cmd{Err: err}
	}
	cmd := sb.Command(ctx, dir, confinedLine(p, env, args))
	if cmd.Err != nil {
		sandbox.Release(cmd)
	}
	return cmd
}

// confinedLine is the shell line running git at path with args: git's
// variables the sandbox passes on unset, then those Abhed sets. The rest of
// the host's environment is the sandbox's to give, so no secret is added here.
func confinedLine(path string, env, args []string) string {
	var b strings.Builder
	b.WriteString(`for v in $(env | sed -n 's/^\(GIT_[A-Za-z0-9_]*\)=.*/\1/p'); do unset "$v"; done; `)
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, "GIT_") {
			continue
		}
		b.WriteString("export " + k + "=" + shellQuote(v) + "; ")
	}
	b.WriteString("exec " + shellQuote(path))
	for _, a := range args {
		b.WriteString(" " + shellQuote(a))
	}
	return b.String()
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Repo runs git on one repository. The drivers its configuration names are
// read once, when it is made, for all the commands of one operation.
type Repo struct {
	Dir     string
	drivers [][2]string
	// gitDir is dir's git folder and common the common one, found from where
	// the git folder is, never from its commondir; "" outside a repository.
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
	return r.commandWith(ctx, config, env, false, args...)
}

// commandWith is CommandWith; host keeps it on the host whatever is registered.
func (r *Repo) commandWith(ctx context.Context, config [][2]string, env []string, host bool, args ...string) *exec.Cmd {
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
	// Checked before a sandbox's command is built, which would hold what it made for one never started.
	if r.unknown != nil {
		return &exec.Cmd{Err: r.unknown}
	}
	if err := r.redirected(); err != nil {
		return &exec.Cmd{Err: err}
	}
	host = host || onHost(args, env)
	if !host && len(args) > 0 && args[0] == "commit" && confinedBy(r.Dir) != nil {
		// A sandbox may give git a home of its own, without the person's name.
		env = append(r.identity(ctx), env...)
	}
	return gitCmd(ctx, r.Dir, host, append(append(r.env(env), configEnv(all)...), env...), append([]string{"-C", r.Dir}, args...)...)
}

// identity is the author and committer git would take for a commit here,
// read on the host, where reading configuration runs nothing.
func (r *Repo) identity(ctx context.Context) []string {
	var out []string
	for _, k := range [][3]string{{"user.name", "GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"}, {"user.email", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"}} {
		v, err := gitCmd(ctx, r.Dir, true, r.env(nil), "-C", r.Dir, "config", "--get", k[0]).Output()
		if s := strings.TrimSpace(string(v)); err == nil && s != "" {
			out = append(out, k[1]+"="+s, k[2]+"="+s)
		}
	}
	return out
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

// gitDirs are dir's git folder and its common one, two up for a linked worktree's,
// never read from a commondir; an error when a .git is there but git cannot name it.
func gitDirs(ctx context.Context, dir string) (string, string, error) {
	out, err := gitCmd(ctx, dir, false, Env(), "-C", dir, "rev-parse", "--absolute-git-dir").Output()
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
	if err := gitFileLinked(dir, gd); err != nil {
		return "", "", err
	}
	if parent := filepath.Dir(gd); strings.EqualFold(filepath.Base(parent), "worktrees") {
		if c := filepath.Dir(parent); isGitDir(c) {
			return gd, c, nil
		}
	}
	return gd, gd, nil
}

// gitFileLinked says why git is not run when the work tree's .git file reaches
// the git folder gd through a link, which a command could repoint at a folder of its own.
func gitFileLinked(dir, gd string) error {
	root := repoRoot(dir)
	if root == "" || runtime.GOOS == "windows" {
		return nil
	}
	f := filepath.Join(root, ".git")
	if info, err := os.Lstat(f); err != nil || !info.Mode().IsRegular() {
		return nil //nolint:nilerr // a .git folder, or none, has no gitdir line to check
	}
	data, err := os.ReadFile(f) // #nosec G304 -- the work tree's own .git file
	if err != nil {
		return fmt.Errorf("%s cannot be read (%w); Abhed runs no git in this repository", f, err)
	}
	// Read as git reads it: the whole file less trailing line ends.
	to, ok := strings.CutPrefix(strings.TrimRight(string(data), "\r\n"), "gitdir: ")
	if !ok {
		return fmt.Errorf("%s names no git folder; Abhed runs no git in this repository", f)
	}
	if !filepath.IsAbs(to) {
		to = filepath.Join(sandbox.RealPath(root), to)
	}
	if to = filepath.Clean(to); sandbox.RealPath(to) != to || to != sandbox.RealPath(gd) {
		return fmt.Errorf("%s names its git folder %s through a symbolic link, which a command could point at a git folder "+
			"of its own; Abhed runs no git in this repository until the link is replaced or git worktree repair is run", f, to)
	}
	return nil
}

// redirected says why git is not run while a commondir is not git's own, by
// sandbox.CommondirIsGits: git reads refs through it whatever GIT_COMMON_DIR says.
func (r *Repo) redirected() error {
	if r.gitDir == "" {
		return nil
	}
	p := filepath.Join(r.gitDir, "commondir")
	_, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		if r.gitDir == r.common {
			return nil
		}
		return fmt.Errorf("%s is missing, so git cannot be told which repository the worktree belongs to; Abhed runs no git there", p)
	}
	if err == nil && r.gitDir != r.common && sandbox.CommondirIsGits(p, r.gitDir, r.common) {
		return nil
	}
	return fmt.Errorf("%s points git at another folder's configuration, hooks and refs, or reaches its own through a link, "+
		"and git never writes one so; a command may have planted it. Abhed runs no git in this repository until it is removed", p)
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
	out, err := gitCmd(ctx, dir, false, pinned(common, nil), "-C", dir, "config", "--includes", "--name-only", "-z", "--get-regexp", `^(filter|merge)\.`).Output()
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

// ConfigFiles are the files git reads for its global and system
// configuration in dir, the ones their include.path and includeIf lines
// bring in followed, and where a system file would be though none is there.
// Reading configuration runs nothing, so this runs on the host.
func ConfigFiles(ctx context.Context, dir string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	add("/etc/gitconfig")
	if p, err := GitPath(dir); err == nil {
		// A git installed under a prefix reads <prefix>/etc/gitconfig.
		add(filepath.Join(filepath.Dir(filepath.Dir(sandbox.RealPath(p))), "etc", "gitconfig"))
	}
	for _, scope := range []string{"--global", "--system"} {
		b, err := gitCmd(ctx, dir, true, Env(), "-C", dir, "config", scope, "--includes", "--show-origin", "--name-only", "-z", "--list").Output()
		if err != nil {
			continue
		}
		// Each entry is origin NUL name NUL.
		parts := bytes.Split(b, []byte{0})
		for i := 0; i+1 < len(parts); i += 2 {
			if f, ok := strings.CutPrefix(string(parts[i]), "file:"); ok {
				add(f)
			}
		}
	}
	return out
}
