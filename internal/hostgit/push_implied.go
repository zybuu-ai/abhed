package hostgit

import (
	"context"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/tools"
)

func init() { tools.HostImpliedLine = ImpliedLine }

// pushImplied are the push flags a repository's own configuration gives a
// plain git push to remote: --mirror for remote.<name>.mirror, --force for
// a configured refspec that forces (+src:dst), --delete for one that
// deletes (:dst). settings is remoteSettings's string.
func pushImplied(settings, remote string) []string {
	var mirror, force, del bool
	prefix := "remote." + strings.ToLower(remote) + "."
	parts := strings.Split(settings, "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		key, value, hasValue := strings.Cut(parts[i+1], "\n")
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		switch strings.TrimPrefix(lower, prefix) {
		case "mirror":
			mirror = !hasValue || configTrue(value)
		case "push":
			v := strings.TrimSpace(value)
			force = force || strings.HasPrefix(v, "+")
			del = del || strings.HasPrefix(v, ":")
		}
	}
	var out []string
	if mirror {
		out = append(out, "--force", "--mirror")
	} else if force {
		out = append(out, "--force")
	}
	if del {
		out = append(out, "--delete")
	}
	return out
}

// configTrue is git's reading of a boolean value.
func configTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "on", "1", "":
		return true
	}
	return false
}

// ImpliedLine is a plain git push line as the repository's configuration
// makes it run, when that forces, deletes or mirrors (git push --force
// --mirror origin), for policy to judge as written; "" for any other line.
// Only ever read to add a deny, an ask or a destructive confirmation.
func ImpliedLine(cwd, line string) string {
	op, err, ok := parseRemote(line)
	if !ok || err != nil || op.verb != "push" || cwd == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root, err := gitOut(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return ""
	}
	settings, err := remoteSettings(ctx, root)
	if err != nil {
		return ""
	}
	names, err := (&HostBroker{}).remotesOf(ctx, cwd, op)
	if err != nil {
		return ""
	}
	var flags []string
	for _, n := range names {
		for _, f := range pushImplied(settings, n) {
			if !containsWord(flags, f) && !containsWord(op.opts, f) {
				flags = append(flags, f)
			}
		}
	}
	if len(flags) == 0 {
		return ""
	}
	rest := append(append([]string{}, op.opts...), op.positional...)
	if op.remote == "" && len(names) == 1 {
		rest = append(rest, names[0])
	}
	return strings.TrimSpace("git push " + strings.Join(flags, " ") + " " + strings.Join(rest, " "))
}

func containsWord(words []string, w string) bool {
	for _, x := range words {
		if x == w {
			return true
		}
	}
	return false
}
