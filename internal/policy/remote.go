package policy

import (
	"encoding/json"
	"strings"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// The ssh tool runs its command through the remote account's shell, so the
// command is read as bash reads a local one: deny and ask rules see every
// command in a chain and behind a runner such as sudo, and an allow rule or a
// read command approves only one simple command.

// remoteCommand is the host and command of an ssh call; ok is false for
// another tool or arguments that name no host.
func remoteCommand(tool string, args json.RawMessage) (host, command string, ok bool) {
	if tool != "ssh" {
		return "", "", false
	}
	m, err := tools.DecodeArgs(args)
	if err != nil {
		return "", "", false
	}
	h, _ := tools.Lookup(m, "host")
	c, _ := tools.Lookup(m, "command")
	host, _ = h.(string)
	command, _ = c.(string)
	return host, command, host != ""
}

// shellRead reads command as a bash parser does: the subjects deny and ask
// rules see, the subjects an allow rule sees, whether an allow rule with a
// pattern may approve it, and whether the split read all of it.
func shellRead(command string, as readAs) (canon tools.Canonical, subjects, allow []string, narrow, complete bool) {
	canon = tools.CanonicalCommand(command)
	switch as {
	case asTyped:
		canon = canon.AtTerminal()
	case asLine:
		canon = canon.AsTerminalLine()
	}
	subjects, complete = commandSegments(command)
	narrow = !hasShellControl(command)
	if canon.Text != command {
		more, whole := commandSegments(canon.Text)
		subjects, complete = append(subjects, more...), complete && whole
	}
	// Under a changed IFS, deny and ask rules also read the words it splits.
	if canon.IFS != "" {
		more, whole := commandSegments(tools.IFSSplitText(canon.Text, canon.IFS))
		subjects, complete = append(subjects, more...), complete && whole
	}
	// And the commands a bash parser finds, in function bodies and in the
	// text eval, trap and -c run, read whole: their quoting is undone.
	subjects = append(subjects, canon.Commands()...)
	allow = []string{command}
	if canon.Joined && !canon.Reworded {
		allow, narrow = []string{canon.Text}, !hasShellControl(canon.Text)
	}
	if canon.Reworded {
		narrow = false
	}
	return canon, subjects, allow, narrow, complete
}

// remoteRead is an ssh call's command read for the rules.
type remoteRead struct {
	host     string
	command  string
	canon    tools.Canonical
	commands []string // each command in it, as deny and ask rules see it
	subjects []string // the commands alone and as host/command
	allow    []string // host/command, as allow rules see it
	narrow   bool
	complete bool
	stdin    bool // the call writes a stored secret to the command's stdin
	// quotedOnly is a command whose only shell control text is inside
	// single quotes, which the shell passes as written.
	quotedOnly bool
}

// withoutSingleQuoted is s with the text inside each pair of single quotes
// dropped; an unclosed quote keeps the rest.
func withoutSingleQuoted(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '\'')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.IndexByte(s[i+1:], '\'')
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString("''")
		s = s[i+j+2:]
	}
}

// readRemote reads an ssh call's command. A line the parser cannot read in
// full is approved by no allow rule or read command, whatever the rules.
func readRemote(host, command string) remoteRead {
	canon, cmds, allow, narrow, complete := shellRead(command, asCommand)
	cmds = withBaseNames(cmds)
	r := remoteRead{host: host, command: command, canon: canon, commands: cmds, complete: complete}
	parsed := !canon.Unparsed() && !canon.Incomplete() && canon.Opaque() == "" && !canon.ProgramHidden()
	r.narrow = narrow && parsed
	// Shell control text only inside single quotes, as a query format's ${…}.
	r.quotedOnly = !narrow && parsed && !canon.Reworded && !hasShellControl(withoutSingleQuoted(command))
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			r.subjects = append(r.subjects, s)
		}
	}
	add(host + "/" + command)
	for _, c := range cmds {
		add(c)
		add(host + "/" + c)
	}
	for _, a := range allow {
		r.allow = append(r.allow, host+"/"+a)
	}
	return r
}

// matchesRemoteFlags is matchesFlagsAny for an ssh rule: its glob is read
// both as a command and as host/command, each reading only adding a match.
func (r Rule) matchesRemoteFlags(rr remoteRead) bool {
	if r.pattern == nil || !r.ForTool("ssh") {
		return false
	}
	match := func(f *flagForm) bool {
		for _, c := range rr.commands {
			if f != nil && f.matches(c) {
				return true
			}
		}
		return false
	}
	if match(r.flags) {
		return true
	}
	h, rest, ok := strings.Cut(r.glob, "/")
	if !ok || h == "" || strings.ContainsAny(h, " \t") {
		return false
	}
	re, err := globToRegexp(h)
	return err == nil && re.MatchString(rr.host) && match(parseFlagForm(rest))
}
