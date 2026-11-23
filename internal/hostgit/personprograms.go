package hostgit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// personProgramsHeld says why a program the person's own git configuration
// or environment names for host git to run (core.sshCommand, or ssh when it
// names none; core.askPass and SSH_ASKPASS; core.gitProxy; each credential
// helper) cannot be trusted as theirs: its path is relative, so it resolves
// from the repository; it, or a folder PATH searches before it, lies in
// areas (the workspace and what commands can write); or it is not the
// person's or root's alone. A shell-form helper (!...) is judged by its
// first word, which must be a program. nil when every one is theirs.
func personProgramsHeld(ctx context.Context, cwd string, areas []string) error {
	last := func(key string) string {
		if v := personValues(ctx, cwd, key); len(v) > 0 {
			return v[len(v)-1][1]
		}
		return ""
	}
	type named struct {
		what, cmd string
		shell     bool
	}
	var progs []named
	ssh := last("core.sshcommand")
	if ssh == "" {
		ssh = "ssh"
	}
	progs = append(progs, named{"core.sshCommand", ssh, false})
	for _, k := range []string{"core.askpass", "core.gitproxy"} {
		if v := last(k); v != "" {
			progs = append(progs, named{k, v, false})
		}
	}
	if v := os.Getenv("SSH_ASKPASS"); v != "" {
		progs = append(progs, named{"SSH_ASKPASS", v, false})
	}
	for _, k := range credentialKeys(ctx, cwd) {
		for _, v := range personValues(ctx, cwd, k) {
			if v[1] != "" {
				progs = append(progs, named{k, helperCommand(ctx, cwd, v[1]), strings.HasPrefix(v[1], "!")})
			}
		}
	}
	for _, p := range progs {
		if why := programUntrusted(p.cmd, areas, p.shell); why != "" {
			return fmt.Errorf("the program your git configuration names for %s (%q) %s", p.what, p.cmd, why)
		}
	}
	return nil
}

// helperCommand is the command git runs for credential helper value h:
// the shell form's command, an absolute path as it is, or git
// credential-<name> found where git looks for its own commands, then PATH.
func helperCommand(ctx context.Context, cwd, h string) string {
	switch {
	case strings.HasPrefix(h, "!"):
		return strings.TrimSpace(h[1:])
	case filepath.IsAbs(strings.Fields(h)[0]):
		return h
	}
	name := "git-credential-" + strings.Fields(h)[0]
	if dir, err := gitOut(ctx, cwd, "--exec-path"); err == nil && dir != "" {
		if p := filepath.Join(dir, name); executable(p) {
			return p
		}
	}
	return name
}

// shells are programs that run a command line given with -c, which Abhed
// cannot judge.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true, "tcsh": true}

// programUntrusted says why what cmd runs is not the person's alone, or ""
// when it is (or names nothing that exists, which runs nothing). Every word
// is judged, and the value after = in each (ssh's -o ProxyCommand=...): a
// relative path is refused, as git would resolve it from where it runs;
// an absolute one must lie outside the areas and be the person's or root's
// alone. shellForm, a credential helper's !..., refuses any shell syntax.
func programUntrusted(cmd string, areas []string, shellForm bool) string {
	if shellForm && strings.ContainsAny(cmd, "$`;|&(){}<>*?[]\n\\") {
		return "is a shell command with more than a program and its words (a function, variable, substitution, pipe or second command), " +
			"so what it runs is not known; name a helper script instead"
	}
	words := strings.Fields(cmd)
	if len(words) == 0 {
		return ""
	}
	first := strings.Trim(words[0], `"'`)
	if strings.ContainsAny(first, "$`;|&(){}<>*?[]") || strings.HasSuffix(first, "()") {
		return "does not start with a program Abhed can find (a shell function, variable or substitution), so what it runs is not known"
	}
	if shells[filepath.Base(first)] && slices.Contains(words[1:], "-c") {
		return "runs a shell command line (-c), so what it runs is not known; name a script instead"
	}
	path, why := programPath(first, areas)
	if why != "" {
		return why
	}
	if path != "" {
		if why := pathUntrusted(path, areas); why != "" {
			return why
		}
	}
	for _, w := range words[1:] {
		w = strings.Trim(w, `"'`)
		vals := []string{w}
		if _, v, ok := strings.Cut(w, "="); ok {
			vals = []string{strings.Trim(v, `"'`)} // an option's value: -oProxyCommand=...
		}
		for _, v := range vals {
			switch {
			case !strings.Contains(v, "/"), strings.Contains(v, "://"): // not a path; a URL is not one
			case !filepath.IsAbs(v):
				return "names " + v + ", a relative path, which would be found from where git runs"
			default:
				if why := pathUntrusted(v, areas); why != "" {
					return "names " + v + ", which " + why
				}
			}
		}
	}
	return ""
}

// programPath is where the program first names is, "" when it is found
// nowhere (so nothing runs), or why it cannot be trusted where it is found.
func programPath(first string, areas []string) (string, string) {
	switch {
	case strings.Contains(first, "/") && !filepath.IsAbs(first):
		return "", "is a relative path, which git would resolve from where it runs"
	case filepath.IsAbs(first):
		return first, ""
	}
	// Found on PATH: no folder searched before it may be one commands write.
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			return "", "is found on a PATH with a relative folder, which git would resolve from where it runs"
		}
		for _, a := range areas {
			if a != "" && within(dir, a) {
				return "", "is looked for on PATH in " + dir + ", which commands can write"
			}
		}
		if p := filepath.Join(dir, first); executable(p) {
			return p, ""
		}
	}
	return "", ""
}

// pathUntrusted says why absolute path p (where it exists) is not the
// person's alone: in one of areas, links followed, or another user's or
// writable by others.
func pathUntrusted(p string, areas []string) string {
	for _, a := range areas {
		if a == "" {
			continue
		}
		for _, q := range []string{p, filepath.Dir(p)} {
			if within(q, a) {
				return "is in " + a + ", which commands can write"
			}
		}
	}
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return untrustedProgram(p)
}

// executable reports a regular file someone may run.
func executable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// credentialKeys are every credential helper key the configuration names.
func credentialKeys(ctx context.Context, cwd string) []string {
	keys := []string{"credential.helper"}
	out, err := gitOut(ctx, cwd, "config", "--includes", "--name-only", "--get-regexp", `^credential\..*\.helper$`)
	if err != nil {
		return keys
	}
	for _, k := range strings.Fields(out) {
		if k != "" && k != "credential.helper" {
			keys = append(keys, k)
		}
	}
	return keys
}
