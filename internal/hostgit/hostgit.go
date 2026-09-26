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
// environment is dropped.
//
// This is a list, and so best effort: a git release that adds a setting
// naming a program is not covered until it is added here. Running these
// commands inside the sandbox is the complete answer, and is tracked apart.
package hostgit

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
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
}

// New reads dir's configuration for the filter and merge drivers it names.
// Reading the configuration runs nothing.
func New(ctx context.Context, dir string) *Repo {
	return &Repo{Dir: dir, drivers: drivers(ctx, dir)}
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
	cmd.Env = append(append(Env(), configEnv(all)...), env...)
	return cmd
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
func drivers(ctx context.Context, dir string) [][2]string {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "config", "--includes", "--name-only", "-z", "--get-regexp", `^(filter|merge)\.`) // #nosec G204 -- fixed arguments
	cmd.Env = Env()
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
