package app

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// Bash "always allow" scopes beyond the policy's own: this exact command, a
// command family (program and subcommand, such as go test or npm run lint),
// that family in this folder, each for the session alone or kept with the
// record. They answer only an ask the policy's default step makes: deny and
// ask rules, the destructive check, the supervisor and the screens decide
// first, so a grant is never broader than policy allows.

// bashGrant is one such scope a person chose.
type bashGrant struct {
	// Command is the exact command; Family the program and subcommand.
	Command string `json:"command,omitempty"`
	Family  string `json:"family,omitempty"`
	// Folder limits it to commands run with the session in that folder.
	Folder string `json:"folder,omitempty"`
	// Session marks one kept for this session alone, never restored.
	Session bool `json:"session,omitempty"`
}

// shellWords are characters that make a line more than one simple command,
// or a word the shell rewrites; no grant covers such a line.
var shellWords = regexp.MustCompile("[;&|<>()`$\\\\\n\r*?{}\\[\\]'\"]")

// familySubs are the programs a family grant may name, with the subcommands
// that build, check or test a project. They run the project's own code, as
// a test does, inside the sandbox; nothing here deletes or publishes.
var familySubs = map[string][]string{
	"go":      {"test", "build", "vet", "fmt", "run", "list"},
	"cargo":   {"test", "build", "check", "clippy", "fmt", "run"},
	"npm":     {"test", "run"},
	"pnpm":    {"test", "run"},
	"yarn":    {"test", "run"},
	"dotnet":  {"test", "build"},
	"mvn":     {"test", "package", "verify", "compile"},
	"gradle":  {"test", "build", "check"},
	"python":  {"-m"},
	"python3": {"-m"},
	"pytest":  nil,
	"node":    {"--test"},
}

// pythonModules are what python -m may run under a family grant.
var pythonModules = map[string]bool{"pytest": true, "unittest": true, "mypy": true, "ruff": true}

// codeFlags hand a program a command, a config or code of its own choosing:
// a family grant never covers a line with one.
var codeFlags = regexp.MustCompile(`(?i)^--?(exec|toolexec|config|script-shell|node-options|require|import|loader|eval|e|c|p|r|overlay|modfile|pgo)(=|$)`)

// commandFields are a line's words, or nil when it is not one simple
// command a grant may cover.
func commandFields(command string) []string {
	command = strings.TrimSpace(command)
	if command == "" || len(command) > 500 || shellWords.MatchString(command) {
		return nil
	}
	fields := strings.Fields(command)
	// An assignment in front, such as LD_PRELOAD=, can make any program run code.
	if len(fields) == 0 || strings.Contains(fields[0], "=") || strings.Contains(fields[0], "/") {
		return nil
	}
	return fields
}

// familyOf is the family a line belongs to, or "" when no family grant may
// cover it.
func familyOf(command string) string {
	fields := commandFields(command)
	if fields == nil {
		return ""
	}
	subs, ok := familySubs[fields[0]]
	if !ok {
		return ""
	}
	for _, a := range fields[1:] {
		if codeFlags.MatchString(a) {
			return ""
		}
	}
	if subs == nil {
		return fields[0]
	}
	if len(fields) < 2 {
		return ""
	}
	sub := fields[1]
	allowed := false
	for _, s := range subs {
		allowed = allowed || s == sub
	}
	if !allowed {
		return ""
	}
	switch {
	case sub == "-m":
		// python -m pytest: the module is the family.
		if len(fields) < 3 || !pythonModules[fields[2]] {
			return ""
		}
		return strings.Join(fields[:3], " ")
	case sub == "run" && (fields[0] == "npm" || fields[0] == "pnpm" || fields[0] == "yarn"):
		// npm run lint: the script is the family, not every script.
		if len(fields) < 3 || strings.HasPrefix(fields[2], "-") {
			return ""
		}
		return strings.Join(fields[:3], " ")
	}
	return fields[0] + " " + sub
}

// covers reports whether g answers an ask for command run in cwd.
func (g bashGrant) covers(command, cwd string) bool {
	if commandFields(command) == nil {
		return false
	}
	if g.Folder != "" && (cwd == "" || !within(filepath.Clean(cwd), filepath.Clean(g.Folder))) {
		return false
	}
	switch {
	case g.Command != "":
		return strings.TrimSpace(command) == g.Command
	case g.Family != "":
		return familyOf(command) == g.Family
	}
	return false
}

// grantOption is a grant offered in a permission request.
type grantOption struct {
	id    string
	label string
	grant bashGrant
}

// bashGrantOptions are the scopes offered for an ask on command run in cwd:
// this exact command for the session and always, its family in this folder
// and always. None for a line a grant could not cover.
func bashGrantOptions(command, cwd, bind string) []grantOption {
	fields := commandFields(command)
	if fields == nil {
		return nil
	}
	exact := strings.TrimSpace(command)
	opts := []grantOption{
		{"session-exact:" + bind, "Allow `" + exact + "` for this session", bashGrant{Command: exact, Session: true}},
		{"always-exact:" + bind, "Always allow exactly `" + exact + "`", bashGrant{Command: exact}},
	}
	if fam := familyOf(command); fam != "" {
		if cwd != "" {
			opts = append(opts, grantOption{"always-folder:" + bind, "Always allow `" + fam + " …` in " + filepath.Base(cwd), bashGrant{Family: fam, Folder: cwd}})
		}
		opts = append(opts, grantOption{"always-family:" + bind, "Always allow `" + fam + " …` commands", bashGrant{Family: fam}})
	}
	return opts
}

// scopeOf is how a grant is recorded as a scope, for the record's readers.
func (g bashGrant) scopeOf() string {
	s := "bash(" + g.Command + ")"
	if g.Family != "" {
		s = "bash(" + g.Family + " *)"
	}
	if g.Folder != "" {
		s += " in " + g.Folder
	}
	return s
}

// grantRecord is the record's payload for a grant.
func (g bashGrant) grantRecord() agent.ScopeGranted {
	return agent.ScopeGranted{Scope: g.scopeOf(), By: agent.ByUser, Command: g.Command, Family: g.Family, Folder: g.Folder, Session: g.Session}
}

// grantable is the command of a bash call whose ask a grant may answer:
// the policy's default ask, where the organisation leaves allow rules to
// people. "" and false otherwise.
func (s *acpSession) grantable(tool string, args json.RawMessage, d abhed.Decision) (string, bool) {
	if tool != "bash" || d.Step != "default" || d.Decision != "ask" {
		return "", false
	}
	if s.inner && s.parts.Config.AllowLocked() {
		return "", false
	}
	command := bashCommandOf(args)
	if commandFields(command) == nil {
		return "", false
	}
	return command, true
}

// bashCommandOf is a bash call's command.
func bashCommandOf(args json.RawMessage) string {
	var a struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(args, &a)
	return a.Command
}

// workDir is the folder the session's commands run in.
func (s *acpSession) workDir() string {
	if s.inner && s.parts.Session != nil {
		return s.parts.Session.WorkDir()
	}
	return s.cwd
}

// grantChosen is the offered grant id names, or nil.
func grantChosen(opts []grantOption, id string) *bashGrant {
	for i := range opts {
		if opts[i].id == id {
			return &opts[i].grant
		}
	}
	return nil
}

// restoreGrant takes back a recorded bash grant as a session continues,
// unless it was for that session alone; false for a payload of another kind.
func (s *acpSession) restoreGrant(payload json.RawMessage) bool {
	var g agent.ScopeGranted
	if json.Unmarshal(payload, &g) != nil || g.Command == "" && g.Family == "" {
		return false
	}
	if !g.Session {
		s.grants = append(s.grants, bashGrant{Command: g.Command, Family: g.Family, Folder: g.Folder})
	}
	return true
}
