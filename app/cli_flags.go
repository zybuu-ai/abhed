// The abhed command: an on-prem deep agent harness that runs where the data
// is.
//
// Usage:
//
//	abhed                      interactive session in the current directory
//	abhed "fix the tests"      interactive, starting with that task
//	abhed -p "fix the tests"   headless; exit code reflects the terminal event
//	abhed init                 write a starter config
//	abhed doctor               check that the configured endpoint works
package app

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// usage prints the synopsis, the subcommands and then the flags.
func (a *App) usage(fs *flag.FlagSet) {
	w := fs.Output()
	fmt.Fprintf(w, "Usage: abhed [flags] [command [args]]\n"+
		"       abhed [flags] \"task\"      interactive, starting with the task (or: abhed -- task words)\n"+
		"       abhed -p [flags] [task]   headless; the task may also come on stdin\n\n")
	fmt.Fprintf(w, "With no command, abhed opens an interactive session in the workspace;\n-p runs one task headless and exits.\n\nCommands:\n")
	for _, c := range subcommands {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.about)
	}
	var own []string
	for name := range a.commands {
		own = append(own, name)
	}
	sort.Strings(own)
	for _, name := range own {
		fmt.Fprintf(w, "  %-10s a command of this edition\n", name)
	}
	fmt.Fprintf(w, "\nFlags:\n")
	fs.PrintDefaults()
	fmt.Fprint(w, exitCodesHelp)
}

// exitCodesHelp is the exit-code table, as docs/guide/10-automation.md has it.
const exitCodesHelp = `
Exit codes: 0 done, 1 error, 2 bad invocation or turn limit, 3 token budget,
4 refused by policy, 5 model retries exhausted, 6 shutdown, 7 deadline,
130 interrupted (SIGINT), 143 terminated (SIGTERM).
`

// printFlag is -p: a switch that makes the run headless, which may also
// carry the task (-p=task). It parses as a boolean flag, so in -p "task" the
// task is a positional argument and flags may still follow it.
type printFlag struct {
	on     bool
	prompt string
}

func (p *printFlag) String() string   { return p.prompt }
func (p *printFlag) IsBoolFlag() bool { return true }
func (p *printFlag) Set(v string) error {
	p.on = true
	if v != "true" {
		p.prompt = v
	}
	return nil
}

// cliFlags is the parsed command line.
type cliFlags struct {
	print           printFlag
	mode            string
	permissionMode  string
	modelID         string
	fallbackModel   string
	workdir         string
	addDirs         string
	maxTurns        int
	maxBudget       int
	format          string
	inputFormat     string
	partial         bool
	verbose         bool
	allow           string
	deny            string
	allowedTools    string
	disallowedTools string
	appendSystem    string
	appendFile      string
	systemPrompt    string
	systemFile      string
	jsonSchema      string
	noStdin         bool
	skipPerms       bool
	showVer         bool
	listenAddr      string
	trustWS         bool

	// words are the positional arguments that are not a subcommand; dashed
	// is set when they came after "--".
	words  []string
	dashed bool
	// sub is the subcommand and its arguments, when there is one.
	sub []string
}

// newFlagSet declares every flag on fs, bound to f.
func newFlagSet(f *cliFlags) *flag.FlagSet {
	fs := flag.NewFlagSet("abhed", flag.ContinueOnError)
	fs.Var(&f.print, "p", "run headless and exit; the task is the argument after it, stdin, or both")
	fs.Var(&f.print, "print", "same as -p")
	fs.StringVar(&f.mode, "mode", "", "permission mode: default|accept-edits|plan|auto|bypass")
	fs.StringVar(&f.permissionMode, "permission-mode", "", "same as -mode; also takes acceptEdits and bypassPermissions")
	fs.StringVar(&f.modelID, "model", "", "provider name from config")
	fs.StringVar(&f.fallbackModel, "fallback-model", "", "comma-separated configured providers to move to, in order, when the model is unreachable or refuses access")
	fs.StringVar(&f.workdir, "C", "", "workspace directory (default: current)")
	fs.StringVar(&f.addDirs, "add-dir", "", "comma-separated extra directories the agent may read and write")
	fs.IntVar(&f.maxTurns, "max-turns", 0, "override the turn limit")
	fs.IntVar(&f.maxBudget, "max-budget-tokens", 0, "end the run once it has used this many tokens (exit 3)")
	fs.StringVar(&f.format, "output-format", "text", "text|json|stream-json: json and stream-json write one event per line, then a result line")
	fs.StringVar(&f.inputFormat, "input-format", "text", "text|stream-json: with -p, stream-json reads one user message per line on stdin")
	fs.BoolVar(&f.noStdin, "no-stdin", false, "with -p, do not read stdin, as with < /dev/null; for a loop that reads a list on stdin")
	fs.BoolVar(&f.partial, "include-partial-messages", false, "with stream-json, include the streamed text fragments")
	fs.BoolVar(&f.verbose, "verbose", false, "show reasoning in full, and each model call's tokens on stderr")
	fs.StringVar(&f.allow, "allow", "", "comma-separated allow rules, e.g. 'bash(go test*)'")
	fs.StringVar(&f.deny, "deny", "", "comma-separated deny rules")
	fs.StringVar(&f.allowedTools, "allowedTools", "", "allow rules with tool names in any case, e.g. 'Read,Bash(npm test:*)'")
	fs.StringVar(&f.allowedTools, "allowed-tools", "", "same as -allowedTools")
	fs.StringVar(&f.disallowedTools, "disallowedTools", "", "deny rules with tool names in any case")
	fs.StringVar(&f.disallowedTools, "disallowed-tools", "", "same as -disallowedTools")
	fs.StringVar(&f.appendSystem, "append-system-prompt", "", "add these instructions to the system prompt (recorded by sha256)")
	fs.StringVar(&f.appendFile, "append-system-prompt-file", "", "add the instructions in this file to the system prompt")
	fs.StringVar(&f.systemPrompt, "system-prompt", "", "replace the system prompt; refused under a managed configuration (recorded by sha256)")
	fs.StringVar(&f.systemFile, "system-prompt-file", "", "replace the system prompt with this file's contents")
	fs.StringVar(&f.jsonSchema, "json-schema", "", "with -p, deliver the answer as JSON matching this schema (inline, or @file)")
	fs.BoolVar(&f.skipPerms, "dangerously-skip-permissions", false, "bypass mode after a confirmation on a terminal; refused under a managed configuration; deny rules still apply")
	fs.BoolVar(&f.showVer, "version", false, "print version and exit")
	fs.StringVar(&f.listenAddr, "addr", ":8080", "listen address for abhed serve")
	fs.BoolVar(&f.trustWS, "trust-workspace", false, "trust the workspace's .abhed/config.json for this run (also "+config.TrustEnv+"=1)")
	return fs
}

// parseArgs parses the command line. Flags may come before and after a
// task; a known subcommand first stops the parse, as it always has, and
// "--" makes every later argument part of the task.
func parseArgs(fs *flag.FlagSet, f *cliFlags, args []string) error {
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		consumed := len(rest) - fs.NArg()
		if consumed > 0 && rest[consumed-1] == "--" {
			f.words = append(f.words, fs.Args()...)
			f.dashed = true
			return nil
		}
		if fs.NArg() == 0 {
			return nil
		}
		first := fs.Arg(0)
		if len(f.words) == 0 && !f.print.on && builtinCommands[first] {
			f.sub = fs.Args()
			return nil
		}
		f.words = append(f.words, first)
		rest = fs.Args()[1:]
	}
}

// task is the prompt the command line gives: the -p value and the words.
func (f *cliFlags) task() string {
	parts := append([]string{}, f.words...)
	if f.print.prompt != "" {
		parts = append([]string{f.print.prompt}, parts...)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// Main runs the command with the given arguments and options and returns
// the exit code. It is what every edition's main calls.
func Main(args []string, opts ...Option) int {
	a := newApp(opts...)
	var f cliFlags
	fs := newFlagSet(&f)
	// Sessions: the record keeps every one, so these pick which goes on.
	args, pickResume := bareResume(args)
	sf := &startFlags
	*sf = sessionFlags{Pick: pickResume}
	fs.BoolVar(&sf.Continue, "c", false, "continue this workspace's most recent session")
	fs.BoolVar(&sf.Continue, "continue", false, "same as -c")
	fs.StringVar(&sf.Resume, "r", "", "resume a session by id, name or file; alone, pick one")
	fs.StringVar(&sf.Resume, "resume", "", "same as -r")
	fs.StringVar(&sf.Name, "n", "", "name the session")
	fs.StringVar(&sf.Name, "name", "", "same as -n")
	fs.BoolVar(&sf.Fork, "fork-session", false, "with -c or -r, go on in a new session branched from it")
	fs.Usage = func() { a.usage(fs) }
	if err := parseArgs(fs, &f, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if f.showVer || (len(f.sub) > 0 && f.sub[0] == "version") {
		// --json is the handshake's block, for an editor checking the engine first.
		if rest := f.sub; len(rest) > 1 && (rest[1] == "--json" || rest[1] == "-json") {
			ws, err := resolveWorkspace(f.workdir)
			if err == nil {
				err = versionJSON(os.Stdout, buildOf(a.version, a.edition), ws, a.trust)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
				return 1
			}
			return 0
		}
		fmt.Println("abhed", a.version, a.edition)
		return 0
	}
	// An unknown format used to print text, so a script asking for another
	// format parsed prose without noticing.
	if !validFormat(f.format) {
		fmt.Fprintf(os.Stderr, "abhed: unknown -output-format %q; use %s\n", f.format, strings.Join(outputFormats, ", "))
		return 2
	}
	if err := sf.check(); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 2
	}
	if f.inputFormat != "text" && f.inputFormat != "stream-json" {
		fmt.Fprintf(os.Stderr, "abhed: unknown -input-format %q; use text or stream-json\n", f.inputFormat)
		return 2
	}
	mode, err := f.permission()
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 2
	}
	f.mode = mode

	workspace, err := resolveWorkspace(f.workdir)
	if err != nil {
		fail(err)
	}
	// Also accepted after the subcommand, as in `abhed serve -trust-workspace`.
	rest := f.sub
	if len(rest) > 1 {
		rest = leadingTrustFlag(rest, &f.trustWS)
	}
	if f.trustWS {
		a.trust = config.TrustGranted
	}
	if len(rest) > 0 {
		return a.subcommand(workspace, rest, f.listenAddr)
	}
	if len(f.words) > 0 && !f.print.on && !f.dashed {
		// An edition's own subcommand.
		if cmd, ok := a.commands[f.words[0]]; ok {
			return cmd(workspace, f.words[1:])
		}
		// A single bare word is far more often a mistyped command than a
		// task, and opening a session for it looked like the command had
		// run. A task is quoted, follows --, or comes with -p.
		if len(f.words) > 1 || !strings.ContainsAny(f.words[0], " \t\n") {
			text := strings.Join(f.words, " ")
			fmt.Fprintf(os.Stderr, "abhed: unknown command %q; did you mean abhed -p %q? "+
				"A task opens a session when quoted (abhed %q) or after --; see abhed -h\n", f.words[0], text, text)
			return 2
		}
	}
	return run(a, workspace, &f)
}

// subcommand runs a built-in subcommand; rest[0] is its name.
func (a *App) subcommand(workspace string, rest []string, listenAddr string) int {
	switch rest[0] {
	case "version":
		return 0 // printed above, before the workspace is needed
	case "init":
		// Trusted as written: the person asked for exactly this content.
		path, err := config.InitWorkspace(workspace)
		if err != nil {
			fail(err)
		}
		fmt.Printf("Wrote %s\nEdit it to point at your model endpoint, then run `abhed doctor`.\n"+
			"Abhed trusts it as written; after an edit, run `abhed trust` to review and trust it again.\n", path)
		return 0
	case "trust":
		return trustCmd(workspace, rest[1:], os.Stdout)
	case "doctor":
		if len(rest) > 1 && (rest[1] == "--json" || rest[1] == "-json") {
			return a.doctorJSON(os.Stdout, workspace)
		}
		return a.doctor(workspace)
	case "providers":
		return providersCmd()
	case "record":
		return recordCmd(workspace, rest[1:], a.trust, os.Stdin, os.Stdout, os.Stderr)
	case "hawkeye":
		return hawkeyeCmd(workspace, rest[1:], a.trust)
	case "migrate":
		return migrateCmd(workspace, rest[1:], a.migrate, a.trust)
	case "resolve":
		return resolveCmd(workspace, rest[1:], a.trust)
	case "acp":
		// The Agent Client Protocol over stdio, for editors that speak it.
		return acpCmd(workspace, buildOf(a.version, a.edition), a.trust)
	case "rpc":
		// Line-delimited JSON on stdin and stdout, so a caller in any language
		// can drive Abhed as a subprocess without running a server.
		return rpcCmd(workspace, a.trust)
	case "user":
		return userCmd(workspace, rest[1:], a.trust)
	case "secret":
		return secretCmd(rest[1:])
	case "index":
		return buildIndexCmd(workspace, a.trust)
	case "eval":
		evalFlags := flag.NewFlagSet("eval", flag.ExitOnError)
		corpus := evalFlags.String("corpus", "internal/eval/corpus", "task corpus directory")
		jsonOut := evalFlags.String("json", "", "write the full report to this path")
		evalTrust := evalFlags.Bool("trust-workspace", false, "trust the workspace's .abhed/config.json for this run")
		_ = evalFlags.Parse(rest[1:])
		if *evalTrust {
			a.trust = config.TrustGranted
		}
		return evalCmd(workspace, *corpus, *jsonOut, a.trust)
	case "serve":
		// Re-parse the remaining args so `abhed serve -addr :9000` works: Go's
		// flag package stops at the first non-flag argument.
		serveFlags := flag.NewFlagSet("serve", flag.ExitOnError)
		serveAddr := serveFlags.String("addr", listenAddr, "listen address")
		serveTrust := serveFlags.Bool("trust-workspace", false, "trust the workspace's .abhed/config.json for this run")
		_ = serveFlags.Parse(rest[1:])
		if *serveTrust {
			a.trust = config.TrustGranted
		}
		return a.serveCmd(workspace, *serveAddr)
	}
	return 2
}

// permission is the mode the flags ask for: -mode, or its familiar
// spelling -permission-mode; both at once must agree. The bypass switch
// sets no mode here: it takes effect only after its confirmation, in run.
func (f *cliFlags) permission() (string, error) {
	mode := f.mode
	if f.permissionMode != "" {
		m, ok := permissionModes[f.permissionMode]
		if !ok {
			return "", fmt.Errorf("unknown -permission-mode %q; use default, acceptEdits, plan, auto or bypassPermissions", f.permissionMode)
		}
		if mode != "" && mode != m {
			return "", fmt.Errorf("-mode %s and -permission-mode %s disagree", mode, f.permissionMode)
		}
		mode = m
	}
	if mode != "" && !validMode(mode) {
		return "", fmt.Errorf("unknown -mode %q; use default, accept-edits, plan, auto or bypass", mode)
	}
	if f.skipPerms && mode != "" && mode != string(policy.ModeBypass) {
		return "", fmt.Errorf("-dangerously-skip-permissions asks for bypass, and the mode flag asks for %s", mode)
	}
	return mode, nil
}

// permissionModes maps -permission-mode's spellings to Abhed's modes.
var permissionModes = map[string]string{
	"default": "default", "acceptEdits": "accept-edits", "accept-edits": "accept-edits",
	"plan": "plan", "auto": "auto", "bypassPermissions": "bypass", "bypass": "bypass",
}

// leadingTrustFlag takes -trust-workspace when it is the first argument
// after a subcommand the registry marks as loading the workspace
// configuration; for any other, an edition's included, it is left alone.
// serve, eval and resolve also parse it among their own flags.
func leadingTrustFlag(args []string, trust *bool) []string {
	takes := false
	for _, c := range subcommands {
		if c.name == args[0] {
			takes = c.trust
		}
	}
	if !takes {
		return args
	}
	switch args[1] {
	case "-trust-workspace", "--trust-workspace", "-trust-workspace=true", "--trust-workspace=true":
		*trust = true
		return append(args[:1:1], args[2:]...)
	}
	return args
}

// outputFormats are the values -output-format takes.
var outputFormats = []string{"text", "json", "stream-json"}

func validFormat(f string) bool {
	for _, v := range outputFormats {
		if f == v {
			return true
		}
	}
	return false
}

// validMode reports whether m is a permission mode -mode accepts.
func validMode(m string) bool {
	switch policy.Mode(m) {
	case policy.ModeDefault, policy.ModeAcceptEdits, policy.ModePlan, policy.ModeAuto, policy.ModeBypass:
		return true
	}
	return false
}

// applyFlags lays the command line over the configuration. The managed
// configuration binds the flags exactly as it binds the SDK's Options.
func applyFlags(cfg config.Config, mode string, maxTurns int, allow, deny, addDirs string) (config.Config, error) {
	return cfg.Apply(config.Overrides{
		Mode: mode, MaxTurns: maxTurns,
		Allow: splitTop(allow), Deny: splitTop(deny), AdditionalDirs: splitRules(addDirs),
	})
}

// toolRules turns a familiar tool list ("Read,Edit,Bash(npm test:*)") into
// Abhed rules ("read", "edit", "bash(npm test*)"). It splits at commas
// outside parentheses; a rule already in Abhed's form comes out unchanged.
func toolRules(list string) []string {
	var out []string
	for _, r := range splitTop(list) {
		out = append(out, toolRule(r))
	}
	return out
}

// splitTop splits a rule list at commas outside parentheses, so a comma in
// a rule's pattern stays in it.
func splitTop(list string) []string {
	var out []string
	depth, start := 0, 0
	emit := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	for i, r := range list {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				emit(list[start:i])
				start = i + 1
			}
		}
	}
	emit(list[start:])
	return out
}

// toolNames maps familiar tool names to Abhed's.
var toolNames = map[string]string{
	"multiedit": "edit", "webfetch": "web_fetch", "websearch": "web_search", "todowrite": "todo",
}

func toolRule(s string) string {
	name, pattern, hasPattern := strings.Cut(s, "(")
	name = strings.ToLower(strings.TrimSpace(name))
	if mapped, ok := toolNames[name]; ok {
		name = mapped
	}
	if !hasPattern {
		return name
	}
	pattern = strings.TrimSuffix(pattern, ")")
	// "npm test:*" is a prefix: everything that starts with "npm test".
	if strings.HasSuffix(pattern, ":*") {
		pattern = strings.TrimSuffix(pattern, ":*") + "*"
	}
	return name + "(" + pattern + ")"
}

// joinRules adds rules in the familiar form to rules in Abhed's.
func joinRules(abhed, familiar string) string {
	return strings.Join(append(splitTop(abhed), toolRules(familiar)...), ",")
}

// budgetFlag applies -max-budget-tokens. Under a managed budget it may only
// lower it.
func budgetFlag(cfg config.Config, tokens int) (config.Config, error) {
	if tokens <= 0 {
		return cfg, nil
	}
	if cfg.ManagedSets("limits.max_budget_tokens") && cfg.Limits.MaxBudgetTokens > 0 && tokens > cfg.Limits.MaxBudgetTokens {
		return cfg, &config.ManagedError{Key: "limits.max_budget_tokens", Value: strconv.Itoa(tokens),
			Reason: fmt.Sprintf("the managed configuration sets it to %d, and a flag may only lower it", cfg.Limits.MaxBudgetTokens), File: managed.ConfigFile}
	}
	cfg.Limits.MaxBudgetTokens = tokens
	return cfg, nil
}
