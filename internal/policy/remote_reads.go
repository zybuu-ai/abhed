package policy

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// AllowRemoteReads declares the commands an ssh call may run as a read
// (ssh.read_commands): an exact command or a pattern for one simple
// command, such as `uptime` or `systemctl status *`. A matching call is
// read-only, so plan mode allows it and no mode asks for it; deny and ask
// rules and the destructive check still come first.
func (e *Engine) AllowRemoteReads(patterns ...string) error {
	for _, p := range patterns {
		r, err := ParseRemoteRead(p)
		if err != nil {
			return err
		}
		e.RemoteReads = append(e.RemoteReads, r)
	}
	return nil
}

// ParseRemoteRead parses one ssh.read_commands entry. One that would run
// more than one simple command, or that the shell reads differently than
// written, is refused: it could never be a single read.
func ParseRemoteRead(p string) (Rule, error) {
	s := strings.TrimSpace(p)
	if s == "" {
		return Rule{}, fmt.Errorf("ssh.read_commands: an entry is empty")
	}
	canon := tools.CanonicalCommand(s)
	if hasShellControl(s) || canon.Reworded || canon.Unparsed() || canon.Incomplete() {
		return Rule{}, fmt.Errorf("ssh.read_commands: %q is not one simple command "+
			"(write an exact command or a pattern such as \"systemctl status *\"; no ; & | < > ( ) ` ${ or newline)", p)
	}
	// The program is named literally: a wildcard there would make any program a read.
	if program := strings.Fields(s)[0]; strings.ContainsAny(program, "*?$") || isAssignment(program) {
		return Rule{}, fmt.Errorf("ssh.read_commands: %q must start with the program it runs, written out (such as \"df -h\")", p)
	}
	// After a runner or a shell a wildcard is any command at all.
	if name := tools.CommandName(path.Base(strings.Fields(s)[0])); strings.ContainsAny(s, "*?") && (isWrapper(name) || remoteShells[name]) {
		return Rule{}, fmt.Errorf("ssh.read_commands: %q puts a wildcard after %s, which runs any command; name the command exactly", p, name)
	}
	if what, destructive := canon.Destructive(); destructive {
		return Rule{}, fmt.Errorf("ssh.read_commands: %q is not a read (%s)", p, what)
	}
	re, err := globToRegexp(s)
	if err != nil {
		return Rule{}, fmt.Errorf("ssh.read_commands: %q: %w", p, err)
	}
	return Rule{raw: s, tool: "ssh", pattern: re, glob: s}, nil
}

// remoteReadRule is the ssh.read_commands entry the call's command matches,
// or "": only one simple command, whose words the text shows, that no
// check reads as destructive.
func (e *Engine) remoteReadRule(rr remoteRead) string {
	if len(e.RemoteReads) == 0 || !rr.narrow || !rr.complete || rr.canon.Hidden || rr.canon.IFSSplit {
		return ""
	}
	if _, destructive := rr.canon.DestructiveWith(e.GitExtensions); destructive {
		return ""
	}
	if rr.canon.GitRunsOrWrites() != "" {
		return ""
	}
	text := rr.command
	if len(rr.allow) == 1 {
		text = strings.TrimPrefix(rr.allow[0], rr.host+"/")
	}
	for _, r := range e.RemoteReads {
		if r.pattern.MatchString(text) {
			return r.raw
		}
	}
	return ""
}

// remoteShells run the text they are given as commands.
var remoteShells = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true, "fish": true, "busybox": true}

func isWrapper(name string) bool {
	_, ok := wrapperWords[name]
	return ok
}

// RemoteReadOf is the ssh.read_commands entry an ssh call runs as a read, or
// "" for a call that is not one.
func (e *Engine) RemoteReadOf(tool string, args json.RawMessage) string {
	host, command, ok := remoteCommand(tool, args)
	if !ok {
		return ""
	}
	return e.remoteReadRule(readRemote(host, command))
}
