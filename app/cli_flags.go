// The abhed command: an on-prem deep agent harness that runs where the data
// is.
//
// Usage:
//
//	abhed                      interactive session in the current directory
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
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// usage prints the synopsis, the subcommands and then the flags.
func (a *App) usage(fs *flag.FlagSet) {
	w := fs.Output()
	fmt.Fprintf(w, "Usage: abhed [flags] [command [args]]\n\n")
	fmt.Fprintf(w, "With no command, abhed opens an interactive session in the workspace;\n-p runs one prompt headless and exits.\n\nCommands:\n")
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
}

// Main runs the command with the given arguments and options and returns
// the exit code. It is what every edition's main calls.
func Main(args []string, opts ...Option) int {
	a := newApp(opts...)
	fs := flag.NewFlagSet("abhed", flag.ContinueOnError)
	var (
		prompt     = fs.String("p", "", "run headless with this prompt and exit")
		mode       = fs.String("mode", "", "permission mode: default|accept-edits|plan|auto|bypass")
		modelID    = fs.String("model", "", "provider name from config")
		workdir    = fs.String("C", "", "workspace directory (default: current)")
		addDirs    = fs.String("add-dir", "", "comma-separated extra directories the agent may read and write")
		maxTurns   = fs.Int("max-turns", 0, "override the turn limit")
		format     = fs.String("output-format", "text", "text|json (json is one event per line)")
		allow      = fs.String("allow", "", "comma-separated allow rules, e.g. 'bash(go test*)'")
		deny       = fs.String("deny", "", "comma-separated deny rules")
		showVer    = fs.Bool("version", false, "print version and exit")
		listenAddr = fs.String("addr", ":8080", "listen address for abhed serve")
		trustWS    = fs.Bool("trust-workspace", false, "trust the workspace's .abhed/config.json for this run (also "+config.TrustEnv+"=1)")
	)
	fs.Usage = func() { a.usage(fs) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVer || fs.Arg(0) == "version" {
		fmt.Println("abhed", a.version, a.edition)
		return 0
	}
	// An unknown format used to print text, so a script asking for another
	// format parsed prose without noticing.
	if !validFormat(*format) {
		fmt.Fprintf(os.Stderr, "abhed: unknown -output-format %q; use %s\n", *format, strings.Join(outputFormats, " or "))
		return 2
	}
	if *mode != "" && !validMode(*mode) {
		fmt.Fprintf(os.Stderr, "abhed: unknown -mode %q; use default, accept-edits, plan, auto or bypass\n", *mode)
		return 2
	}

	workspace, err := resolveWorkspace(*workdir)
	if err != nil {
		fail(err)
	}
	// Also accepted after the subcommand, as in `abhed serve -trust-workspace`.
	rest := fs.Args()
	if len(rest) > 1 {
		rest = leadingTrustFlag(rest, trustWS)
	}
	if *trustWS {
		a.trust = config.TrustGranted
	}

	switch fs.Arg(0) {
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
		return a.doctor(workspace)
	case "providers":
		return providersCmd()
	case "hawkeye":
		return hawkeyeCmd(workspace, rest[1:], a.trust)
	case "migrate":
		return migrateCmd(workspace, a.migrate, a.trust)
	case "resolve":
		return resolveCmd(workspace, rest[1:], a.trust)
	case "acp":
		// The Agent Client Protocol over stdio, for editors that speak it.
		return acpCmd(workspace, a.version, a.trust)
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
		serveAddr := serveFlags.String("addr", *listenAddr, "listen address")
		serveTrust := serveFlags.Bool("trust-workspace", false, "trust the workspace's .abhed/config.json for this run")
		_ = serveFlags.Parse(rest[1:])
		if *serveTrust {
			a.trust = config.TrustGranted
		}
		return a.serveCmd(workspace, *serveAddr)
	default:
		// An edition's own subcommand. Any other word is an error: opening a
		// session for a mistyped command looked like the command had run.
		if cmd, ok := a.commands[fs.Arg(0)]; ok {
			return cmd(workspace, rest[1:])
		}
		if fs.NArg() > 0 {
			fmt.Fprintf(os.Stderr, "abhed: unknown command %q; run a prompt with -p \"...\", or see abhed -h\n", fs.Arg(0))
			return 2
		}
	}

	return run(a, workspace, *prompt, *mode, *modelID, *maxTurns, *format, *allow, *deny, *addDirs)
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

// outputFormats are the values -output-format takes. json is one event per
// line, so it already streams.
var outputFormats = []string{"text", "json"}

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
		Allow: splitRules(allow), Deny: splitRules(deny), AdditionalDirs: splitRules(addDirs),
	})
}
