package hostgit

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/tools"
)

func init() { tools.HostImpliedLine = ImpliedLine }

// pushEffect is what a remote's configuration makes a git push that names no
// refspec do: mirror every ref, push refspecs that force, or delete refs.
type pushEffect struct {
	remote string
	mirror bool
	// forced are configured refspecs that force (+src:dst); deleted are the
	// refs configured refspecs delete (:dst), as dst.
	forced, deleted []string
	// person is set when any of it comes from the person's own files (global
	// or system), not the repository's.
	person bool
	// specs are every configured push refspec, as written.
	specs []string
}

func (e pushEffect) destructive() bool { return e.mirror || len(e.forced) > 0 || len(e.deleted) > 0 }

// pushEffectOf reads remote's push settings from settings, remoteSettings's
// string, with git's own reading of a boolean.
func pushEffectOf(settings, remote string) pushEffect {
	e := pushEffect{remote: remote}
	prefix := "remote." + strings.ToLower(remote) + "."
	parts := strings.Split(settings, "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		scope := parts[i]
		key, value, hasValue := strings.Cut(parts[i+1], "\n")
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		mine := scope == "global" || scope == "system"
		switch strings.TrimPrefix(lower, prefix) {
		case "mirror":
			// The last value wins, as git reads it.
			e.mirror = gitBool(value, hasValue)
			if e.mirror && mine {
				e.person = true
			}
		case "push":
			v := strings.TrimSpace(value)
			e.specs = append(e.specs, v)
			switch {
			case strings.HasPrefix(v, "+"):
				e.forced = append(e.forced, v)
			case strings.HasPrefix(v, ":") && len(v) > 1:
				e.deleted = append(e.deleted, v[1:])
			default:
				continue
			}
			if mine {
				e.person = true
			}
		}
	}
	return e
}

// gitBool is git's reading of a configuration boolean (git config
// --type=bool): a key with no value is true and an empty value false; true,
// yes and on, false, no and off in any case; otherwise an integer, in
// decimal, octal or hex, with a k, m or g unit, true when not zero. A value
// git cannot read makes git stop, so nothing runs; it is read as true, so
// a push is never taken as safer than it is.
func gitBool(value string, hasValue bool) bool {
	if !hasValue {
		return true
	}
	v := strings.TrimSpace(value)
	switch strings.ToLower(v) {
	case "":
		return false
	case "true", "yes", "on":
		return true
	case "false", "no", "off":
		return false
	}
	n := v
	if last := strings.ToLower(n[len(n)-1:]); last == "k" || last == "m" || last == "g" {
		n = n[:len(n)-1]
	}
	if strings.ContainsRune(n, '_') {
		return true // not git's form: it stops; never safer
	}
	i, err := strconv.ParseInt(n, 0, 64)
	if err != nil {
		return true // as above
	}
	return i != 0
}

// describe says what the configuration makes a push do, for the person.
func (e pushEffect) describe() string {
	var what []string
	if e.mirror {
		what = append(what, "mirror every ref (remote."+e.remote+".mirror)")
	}
	if len(e.forced) > 0 {
		what = append(what, "force "+strings.Join(e.forced, ", ")+" (remote."+e.remote+".push)")
	}
	if len(e.deleted) > 0 {
		what = append(what, "delete "+strings.Join(e.deleted, ", ")+" on the remote (remote."+e.remote+".push)")
	}
	whose := "the repository's configuration"
	if e.person {
		whose = "your git configuration"
	}
	return whose + " makes it " + strings.Join(what, ", and ")
}

// lines are the push lines the effect runs as, for policy to judge.
func (e pushEffect) lines() []string {
	switch {
	case e.mirror:
		return []string{"git push --force --mirror " + e.remote}
	}
	var out []string
	if len(e.forced) > 0 || len(e.deleted) == 0 {
		var keep []string
		for _, s := range e.specs {
			if !(strings.HasPrefix(s, ":") && len(s) > 1) {
				keep = append(keep, s)
			}
		}
		if len(e.forced) > 0 {
			out = append(out, strings.TrimSpace("git push --force "+e.remote+" "+strings.Join(keep, " ")))
		}
	}
	if len(e.deleted) > 0 {
		out = append(out, "git push --delete "+e.remote+" "+strings.Join(e.deleted, " "))
	}
	return out
}

// effects are the destructive push effects of op's remotes, when op names
// no refspec of its own (one named replaces the configured ones, and git
// refuses a mirror with one).
func effects(settings string, names []string, op remoteOp) []pushEffect {
	if len(op.positional) > 1 {
		return nil
	}
	var out []pushEffect
	for _, n := range names {
		if e := pushEffectOf(settings, n); e.destructive() {
			out = append(out, e)
		}
	}
	return out
}

// ImpliedLine is a git push line as the configuration makes it run, when
// that mirrors, forces or deletes (git push --force --mirror origin, git
// push --delete origin refs/heads/x), for policy to judge as written; ""
// for any other line. Only ever read to add a deny, an ask or a
// destructive confirmation.
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
	var lines []string
	for _, e := range effects(settings, names, op) {
		for _, l := range e.lines() {
			if len(op.opts) > 0 {
				l = strings.Replace(l, "git push ", "git push "+strings.Join(op.opts, " ")+" ", 1)
			}
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, " && ")
}

// pushSettingsOf are remote's push and mirror settings, in order, for
// binding a grant to them.
func pushSettingsOf(settings, remote string) []string {
	var out []string
	prefix := "remote." + strings.ToLower(remote) + "."
	parts := strings.Split(settings, "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		key, _, _ := strings.Cut(parts[i+1], "\n")
		switch strings.ToLower(key) {
		case prefix + "push", prefix + "mirror":
			out = append(out, parts[i]+"\x00"+parts[i+1])
		}
	}
	return out
}
