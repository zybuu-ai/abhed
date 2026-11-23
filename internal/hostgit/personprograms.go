package hostgit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	type named struct{ what, cmd string }
	var progs []named
	ssh := last("core.sshcommand")
	if ssh == "" {
		ssh = "ssh"
	}
	progs = append(progs, named{"core.sshCommand", ssh})
	for _, k := range []string{"core.askpass", "core.gitproxy"} {
		if v := last(k); v != "" {
			progs = append(progs, named{k, v})
		}
	}
	if v := os.Getenv("SSH_ASKPASS"); v != "" {
		progs = append(progs, named{"SSH_ASKPASS", v})
	}
	for _, k := range credentialKeys(ctx, cwd) {
		for _, v := range personValues(ctx, cwd, k) {
			if v[1] != "" {
				progs = append(progs, named{k, helperCommand(ctx, cwd, v[1])})
			}
		}
	}
	for _, p := range progs {
		if why := programUntrusted(p.cmd, areas); why != "" {
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

// programUntrusted says why the program cmd runs is not the person's alone,
// or "" when it is (or names nothing that exists, which runs nothing).
func programUntrusted(cmd string, areas []string) string {
	words := strings.Fields(cmd)
	if len(words) == 0 {
		return ""
	}
	first := strings.Trim(words[0], `"'`)
	if strings.ContainsAny(first, "$`;|&(){}<>*?[]") || strings.HasSuffix(first, "()") {
		return "does not start with a program Abhed can find (a shell function, variable or substitution), so what it runs is not known"
	}
	var path string
	switch {
	case strings.Contains(first, "/") && !filepath.IsAbs(first):
		return "is a relative path, which git would resolve from the repository"
	case filepath.IsAbs(first):
		path = first
	default:
		// Found on PATH: no folder searched before it may be one commands write.
		for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
			if dir == "" || !filepath.IsAbs(dir) {
				return "is found on a PATH with a relative folder, which git would resolve from the repository"
			}
			for _, a := range areas {
				if a != "" && within(dir, a) {
					return "is looked for on PATH in " + dir + ", which commands can write"
				}
			}
			if p := filepath.Join(dir, first); executable(p) {
				path = p
				break
			}
		}
		if path == "" {
			return ""
		}
	}
	for _, a := range areas {
		if a == "" {
			continue
		}
		for _, p := range []string{path, filepath.Dir(path)} {
			if within(p, a) {
				return "is in " + a + ", which commands can write"
			}
		}
	}
	return untrustedProgram(path)
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
