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
var shellWords = regexp.MustCompile("[;&|<>()`$~\\\\\n\r*?{}\\[\\]'\"]")

// familySubs are the programs a family grant may name, with the subcommands
// that build, check or test a project. They run the project's own code, as
// a test does, inside the sandbox; nothing here deletes or publishes, as no
// further Maven goal or Gradle task is covered with them.
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

// familyFlags are the flags each family may carry under a grant, case as
// the tool reads it: ones that select, filter, repeat or report, never one
// that names another program, script, settings file, project or output
// path. A flag not listed leaves the line to be asked about. The value of a
// listed flag, given after = or as the next word, is held to the same path
// rules as every word.
var familyFlags = map[string][]string{
	"go test":   {"-v", "-run", "-count", "-race", "-short", "-cover", "-covermode", "-coverpkg", "-timeout", "-p", "-parallel", "-bench", "-benchmem", "-benchtime", "-failfast", "-json", "-cpu", "-tags", "-list", "-skip", "-shuffle", "-vet", "-trimpath"},
	"go build":  {"-v", "-race", "-tags", "-trimpath", "-p", "-a", "-n", "-x"},
	"go vet":    {"-v", "-tags", "-json"},
	"go fmt":    {"-n", "-x"},
	"go run":    {"-race", "-tags", "-trimpath"},
	"go list":   {"-m", "-json", "-deps", "-test", "-e", "-u", "-versions", "-f"},
	"cargo":     {"-p", "--package", "--release", "--all-features", "--features", "-F", "--no-default-features", "--workspace", "--lib", "--bins", "--bin", "--tests", "--test", "--examples", "--example", "--all-targets", "--doc", "--no-fail-fast", "-q", "--quiet", "-v", "--verbose", "-j", "--jobs", "--locked", "--offline", "--frozen", "--all", "--check", "--no-run"},
	"npm":       {"-s", "--silent", "-q", "--quiet", "--if-present"},
	"dotnet":    {"-c", "--configuration", "-f", "--framework", "--no-restore", "--no-build", "--filter", "-v", "--verbosity", "--nologo"},
	"mvn":       {"-q", "--quiet", "-B", "--batch-mode", "-o", "--offline", "-U", "-T", "-e", "-pl", "--projects", "-am", "--also-make", "-P", "-DskipTests", "-DskipITs", "-Dtest", "-Dit.test", "-Dsurefire.failIfNoSpecifiedTests"},
	"gradle":    {"-q", "--quiet", "--info", "--stacktrace", "--tests", "-x", "--exclude-task", "--continue", "--parallel", "--offline", "--no-daemon", "--rerun-tasks"},
	"pytest":    {"-q", "-v", "-vv", "-x", "-k", "-m", "-s", "--maxfail", "--tb", "--lf", "--ff", "-n", "--durations", "-ra", "-rA", "--no-header", "--cov", "--cov-report"},
	"unittest":  {"-v", "-q", "-f", "-k", "discover", "-s", "-t"},
	"mypy":      {"--strict", "--ignore-missing-imports", "--no-incremental", "-p", "--package", "-m", "--module", "--pretty"},
	"ruff":      {"check", "format", "--fix", "--diff", "--select", "--ignore", "--statistics", "--quiet", "-q"},
	"node test": {"--test-name-pattern", "--test-only", "--test-concurrency", "--experimental-test-coverage"},
}

// grantValueFlags are listed flags whose value may come as the next word; that
// word is then the flag's value, held to the path rules, not a goal, task or
// package.
var grantValueFlags = map[string]bool{"-run": true, "-count": true, "-covermode": true, "-coverpkg": true, "-timeout": true, "-p": true,
	"-parallel": true, "-bench": true, "-benchtime": true, "-cpu": true, "-tags": true, "-list": true, "-skip": true, "-shuffle": true,
	"-vet": true, "--package": true, "--features": true, "-F": true, "--bin": true, "--test": true, "--example": true, "-j": true,
	"--jobs": true, "-c": true, "--configuration": true, "-f": true, "--framework": true, "--filter": true, "--verbosity": true,
	"-T": true, "-pl": true, "--projects": true, "-P": true, "--tests": true, "--exclude-task": true, "-k": true,
	"--maxfail": true, "--tb": true, "--durations": true, "--select": true, "--ignore": true, "--test-name-pattern": true,
	"--test-concurrency": true, "--module": true}

// gradleValueFlags are gradle's flags whose next word is a value, where -x
// names a task to leave out.
var gradleValueFlags = map[string]bool{"-x": true, "--exclude-task": true, "--tests": true}

// goodModValues are the -mod settings that fetch nothing.
var goodModValues = map[string]bool{"readonly": true, "vendor": true}

// passThrough are the families whose words after `--` go to the project's
// own program (the test binary, the script), not the tool: those words are
// held to the path rules alone.
var passThrough = map[string]bool{"cargo": true, "npm": true}

// flagsFor is the allowed-flag list a family key reads.
func flagsFor(prog, sub string) (string, []string) {
	switch prog {
	case "go":
		return "go " + sub, familyFlags["go "+sub]
	case "cargo":
		return "cargo", familyFlags["cargo"]
	case "npm", "pnpm", "yarn":
		return "npm", familyFlags["npm"]
	case "dotnet", "mvn", "gradle", "pytest":
		return prog, familyFlags[prog]
	case "node":
		return "node test", familyFlags["node test"]
	}
	return "", nil
}

// safeWord reports a word, or a flag's value, that names nothing outside
// the session's folder: in no part of it, split at each "=" and ":", an
// absolute path or a step out with "..", and no module@version anywhere,
// which fetches code.
func safeWord(w string) bool {
	if strings.Contains(w, "@") {
		return false
	}
	for _, seg := range strings.FieldsFunc(w, func(r rune) bool { return r == '=' || r == ':' }) {
		if strings.HasPrefix(seg, "/") {
			return false
		}
		for _, part := range strings.Split(seg, "/") {
			if part == ".." {
				return false
			}
		}
	}
	return true
}

// goPackage reports a go package argument that stays in the module: ".",
// a relative path (./x, ./...), or a pattern whose first element has no dot,
// such as all or std; never an import path that names a host.
func goPackage(w string) bool {
	if w == "." || strings.HasPrefix(w, "./") {
		return true
	}
	first, _, _ := strings.Cut(w, "/")
	return first != "" && !strings.Contains(first, ".")
}

// flagsAllowed reports whether every word of args is a listed flag (or one
// with a listed name and a value) or a safe word, honouring `--`. For go a
// word that is not a flag's value is a package, kept to the module; for
// mvn and gradle it would be another goal or task, so none is covered.
func flagsAllowed(key string, allowed, args []string) bool {
	prog := strings.Fields(key)[0]
	values := grantValueFlags
	if prog == "gradle" {
		values = gradleValueFlags
	}
	expectValue := false
	for i, a := range args {
		if !safeWord(a) {
			return false
		}
		// Maven reads a word with a colon as a plugin goal to fetch and run.
		if prog == "mvn" && strings.Contains(a, ":") {
			return false
		}
		if a == "--" {
			if !passThrough[prog] {
				return false
			}
			for _, rest := range args[i+1:] {
				if !safeWord(rest) {
					return false
				}
			}
			return true
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			switch {
			case expectValue:
				expectValue = false
			case prog == "mvn" || prog == "gradle":
				return false
			case prog == "go" && !goPackage(a):
				return false
			}
			continue
		}
		expectValue = false
		name, value, hasValue := strings.Cut(a, "=")
		listed := false
		for _, f := range allowed {
			// -Dtest=Foo is listed as -Dtest; -run=X as -run.
			listed = listed || f == name
		}
		if !listed {
			// -mod is covered only where it fetches nothing.
			if prog != "go" || name != "-mod" || !hasValue || !goodModValues[value] {
				return false
			}
			continue
		}
		expectValue = !hasValue && values[name]
	}
	return true
}

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
	// No scope, exact or family, for a line naming a path outside the
	// folder or a module@version: it asks every time.
	for _, a := range fields[1:] {
		if !safeWord(a) {
			return nil
		}
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
	if subs == nil {
		// pytest alone: its flags are pytest's.
		if !flagsAllowed("pytest", familyFlags["pytest"], fields[1:]) {
			return ""
		}
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
		// python -m pytest: the module is the family, and its flags are its own.
		if len(fields) < 3 || !pythonModules[fields[2]] || !flagsAllowed(fields[2], familyFlags[fields[2]], fields[3:]) {
			return ""
		}
		return strings.Join(fields[:3], " ")
	case sub == "run" && (fields[0] == "npm" || fields[0] == "pnpm" || fields[0] == "yarn"):
		// npm run lint: the script is the family, not every script.
		// npm run env prints the command's environment, which is no project's check.
		if len(fields) < 3 || strings.HasPrefix(fields[2], "-") || fields[2] == "env" || !safeWord(fields[2]) || !flagsAllowed("npm", familyFlags["npm"], fields[3:]) {
			return ""
		}
		return strings.Join(fields[:3], " ")
	}
	key, flags := flagsFor(fields[0], sub)
	if key == "" || !flagsAllowed(key, flags, fields[2:]) {
		return ""
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
