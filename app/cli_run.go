package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/internal/webfetch"
	"golang.org/x/term"
)

func run(a *App, workspace string, f *cliFlags) int {
	// First, before any setup: a stop signal during start-up ends the run
	// with 128 plus its number (130, 143, 129), as a shell reports it, not
	// by the signal's default action.
	stopper := cancelOnStop(stopReturns)
	defer stopper.stop()
	ctx := stopper.ctx

	headless := f.print.on
	if code := checkHeadlessFlags(f); code != 0 {
		return code
	}
	schema, err := schemaFlag(f.jsonSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 2
	}

	if !headless && firstRunNeeded(workspace) {
		if err := firstRun(ctx, os.Stdin, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: setup ended (%v); starting with the defaults\n", err)
		}
	}
	cfg, err := loadSession(workspace, a.trust, !headless)
	if err != nil {
		fail(err)
	}
	registerState(cfg, workspace)
	if f.modelID != "" {
		cfg.Model.Default = f.modelID
	}
	if f.skipPerms {
		if err := confirmBypass(cfg, os.Stdin, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 2
		}
		f.mode = string(policy.ModeBypass)
	}
	if cfg, err = applyFlags(cfg, f.mode, f.maxTurns, joinRules(f.allow, f.allowedTools), joinRules(f.deny, f.disallowedTools), f.addDirs); err != nil {
		fail(err)
	}
	if cfg, err = budgetFlag(cfg, f.maxBudget); err != nil {
		fail(err)
	}
	sysPrompt, err := systemPromptFlags(f, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 2
	}

	provider, err := cfg.Provider()
	if err != nil {
		fail(err)
	}
	if err := vaultLoads(); err != nil {
		fail(err)
	}

	// Checked off the start-up path; a model call fails at once while the
	// endpoint is known to be down.
	// The interactive session runs the check itself, to say what it found.
	probe := newEndpointProbe(provider)
	if headless {
		go func() { _ = probe.run(ctx) }()
	}
	var adapter model.Adapter = gatedAdapter{Adapter: buildAdapter(provider), probe: probe}
	// A fallback only ever moves to another configured provider, recorded.
	chain, warns := fallbackChain(cfg, f.fallbackModel)
	for _, w := range warns {
		warnf("%s", w)
	}
	var fallback *fallbackAdapter
	if len(chain) > 0 {
		fallback = newFallbackAdapter(cfg.Model.Default, adapter, chain, func(name string) (model.Adapter, error) {
			p, err := cfg.ProviderNamed(name)
			if err != nil {
				return nil, err
			}
			return newAdapter(p)
		})
		adapter = fallback
	}
	sess, err := tools.NewSession(workspace)
	if err != nil {
		fail(err)
	}
	if sess.Syntax, err = tools.ParseSyntaxMode(cfg.Tools.SyntaxCheck); err != nil {
		fail(err)
	}

	pol := policy.New(policy.Mode(orDefault(cfg.Permissions.Mode, "default")))
	pol.AskReadOnly = webfetch.AskReadOnly(cfg.WebFetch.Enabled, cfg.WebFetch.AllowedHosts)
	pol.Managed = cfg.Managed
	pol.Roots = sess.PolicyRoots
	must(pol.AddDeny(cfg.Permissions.Deny...))
	must(pol.AddAsk(cfg.Permissions.Ask...))
	must(pol.AddAllow(cfg.Permissions.Allow...))

	sb, err := startSandbox(cfg, workspace)
	if err != nil {
		fail(err)
	}
	// With a floor the answer is at least the process tier, never none.
	if sb.floor == "" && sb.Tier() == sandbox.TierNone {
		fmt.Fprintf(os.Stderr, "abhed: warning: %s\n", sb.Describe())
	}
	tier := string(sb.floor)
	if tier == "" {
		tier = string(sb.Tier())
	}

	// Custom providers are registered before any provider is resolved, so a
	// name from configuration is usable as model.default.
	for _, err := range cfg.RegisterCustomProviders() {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
	}

	// The tool set every surface builds the same way; the CLI takes all of it.
	vault := openVault()
	set := toolset.Build(context.Background(), cfg, toolset.Options{
		Workspace: workspace,
		Bash: tools.Bash{Sandbox: sb.Command, Secrets: vault.Env, SecretNames: vaultNames(vault),
			Isolation: tools.Isolation{Tier: tier, Network: cfg.Sandbox.AllowNetwork},
			RanUnder:  func() string { return string(sb.Tier()) }},
		Parts: toolset.All,
		Vault: vault,
		Warn:  warnf,
	})
	defer set.Close()
	// A skill's own directory is reachable: its instructions reference files beside them.
	if err := grantDirs(sess, cfg, set.SkillDirs()); err != nil {
		fail(err)
	}
	toolset.Police(set.Extensions, pol, "session")

	// The prompt is set once the tools are known, so it names only those there.
	loopCfg := toolset.LoopConfig(cfg, "")

	// Subagents share the parent's budget, so a fan-out cannot multiply spend
	// invisibly. No Approver: a subagent answers to the approver of the loop
	// that spawned it, the person at the prompt or the headless refuser.
	budget := toolset.Budget(cfg)
	factory := &agent.SubagentFactory{
		Adapter: adapter, Policy: pol,
		Session: sess, Budget: budget, Config: loopCfg, Workspace: workspace,
		Redact: vault.Redactor(), Definitions: set.Agents, Background: true,
		// A subagent may run on another configured model, never an endpoint.
		Models: toolset.ModelResolver(cfg), ModelNames: toolset.OfferedModels(cfg),
	}
	registry := toolset.Subagents(set.Registry, factory, cfg.Limits.MaxParallelSubagents)
	loopCfg.SystemPrompt = sysPrompt.apply(toolset.SystemPrompt(workspace, adapter, set.SkillListing, registry.Names()))
	start := map[string]any{"surface": "cli", "headless": headless, "provider": cfg.Model.Default, "model": provider.Model}
	sysPrompt.record(start)

	// The CLI uses whatever the config selects. Previously this was hardcoded
	// to memory, so a Postgres-configured deployment silently lost its CLI
	// sessions while server sessions persisted — an inconsistency the user
	// would only discover when an audit came up empty.
	store, closeStore, err := openStore(context.Background(), cfg)
	if err != nil {
		fail(err)
	}
	defer closeStore()
	factory.Store = store
	// stdout is resolved on each write rather than captured here: the
	// interactive path replaces os.Stdout once the line editor takes the
	// terminal, and a writer bound to the original file misses the newline
	// translation raw mode needs.
	renderer := ui.NewRenderer(ui.LazyStdout{}, f.format != "text")
	renderer.Reasoning = f.verbose

	var approver agent.Approver
	if headless {
		// No TTY to ask. In auto mode the operator has already delegated the
		// decision to the policy engine, so anything reaching the approver is
		// something policy chose not to allow outright — approving it here
		// would defeat the mode's own rules. In every other mode a headless run
		// cannot obtain consent, so it refuses.
		//
		// Either way the model must be told WHY, or it retries blindly: the
		// first real run against a local model spent 20 turns re-phrasing the
		// same rejected command.
		approver = agent.AutoApprove{Yes: false}
	} else {
		// LazyStdout, not os.Stdout: the approver is built before
		// editor.Capture() replaces os.Stdout with the raw-mode pipe that adds
		// the carriage return \n needs. A writer bound to the original file
		// here writes bare \n into a raw terminal and staircases the prompt —
		// the same reason the renderer above resolves os.Stdout at write time.
		approver = ui.NewApprover(ui.LazyStdout{})
	}

	if headless {
		o := headlessOpts{format: f.format, partial: f.partial, verbose: f.verbose, schema: schema, start: start,
			providerName: cfg.Model.Default, provider: provider, fallback: fallback}
		prompt := f.task()
		if f.inputFormat == "stream-json" {
			o.inputs = streamInputs(os.Stdin, os.Stderr)
		} else if prompt, err = headlessTask(ctx, prompt, os.Stdin, stdinIsPipe(), os.Stderr); err != nil {
			if code, stopped := stopCode(ctx); stopped {
				return code
			}
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 2
		}
		if prompt == "" && o.inputs == nil {
			fmt.Fprintln(os.Stderr, "abhed: -p has no task: give it after -p, or on stdin")
			return 2
		}
		return runOnce(ctx, store, renderer, o, adapter, registry, pol, approver, sess, loopCfg, cfg, prompt, budget, set.Extensions)
	}
	return interactive(ctx, a, store, renderer, adapter, registry, pol, approver, sess, loopCfg, cfg, provider, workspace, budget, set.Extensions,
		interactiveStart{first: f.task(), sandbox: sb, probe: probe, onOpen: func(rec *agent.Recorder) {
			recordStart(rec, start)
			if fallback != nil {
				fallback.SetRecord(recordFallback(rec))
			}
		}})
}

func webSearchLabel(cfg config.Config) string {
	if !cfg.WebSearch.Enabled {
		return "disabled"
	}
	p := cfg.WebSearch.Provider
	if p == "" {
		p = "duckduckgo"
	}
	return p + " (agent can reach the public internet)"
}

func webFetchLabel(cfg config.Config) string {
	switch {
	case !cfg.WebFetch.Enabled:
		return "disabled"
	case len(cfg.WebFetch.AllowedHosts) > 0:
		return "enabled for " + strings.Join(cfg.WebFetch.AllowedHosts, ", ")
	}
	return "enabled (agent can read any public web page)"
}

// buildSandbox selects an execution backend meeting the configured minimum
// tier. Select never silently downgrades, so a failure here is a real
// configuration problem the operator must see.
func buildSandbox(cfg config.Config, workspace string) (sandbox.Sandbox, error) {
	return sandboxconfig.Build(cfg, workspace)
}

// grantDirs widens the session's reachable set from config and the --add-dir
// flag. Both are operator input: nothing the model says reaches this, which is
// the whole point of the boundary.
//
// Skill directories are reachable by construction: a skill's instructions
// routinely say "run the script in scripts/run.sh", and denying the read of a
// file the operator installed deliberately sends the agent into a loop it
// cannot escape. These are operator-configured paths, not workspace content,
// so this widens nothing the operator did not choose.
func grantDirs(sess *tools.Session, cfg config.Config, skillDirs []string) error {
	dirs := append(append([]string{}, cfg.AdditionalDirs...), skillDirs...)
	for _, d := range dirs {
		if err := sess.AddRoot(d); err != nil {
			return fmt.Errorf("--add-dir: %w", err)
		}
	}
	return nil
}

func resolveWorkspace(dir string) (string, error) {
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = cwd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, nil
}

func splitRules(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func must(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
	os.Exit(1)
}

// warnf reports something that failed and was left out, on stderr.
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "abhed: "+format+"\n", args...)
}

// extensionsLabel describes the configured extensions for the serve banner,
// and names those not running.
func extensionsLabel(cfg config.Config, set *toolset.Set) (string, []string) {
	status := toolset.ExtensionStatus(cfg, set.Extensions)
	if len(status) == 0 {
		return "", nil
	}
	var running, failed []string
	for _, e := range cfg.Extensions {
		if status[e.Name] == toolset.ExtensionRunning {
			running = append(running, e.Name)
		} else {
			failed = append(failed, e.Name)
		}
	}
	line := strings.Join(running, ", ")
	if len(failed) > 0 {
		if line != "" {
			line += " · "
		}
		line += "NOT RUNNING: " + strings.Join(failed, ", ")
	}
	return line + " (one process each, seeing every user's calls)", failed
}

// checkHeadlessFlags refuses flags that mean something only with -p, or
// only with another flag, before anything is loaded.
func checkHeadlessFlags(f *cliFlags) int {
	bad := func(msg string) int {
		fmt.Fprintf(os.Stderr, "abhed: %s\n", msg)
		return 2
	}
	switch {
	case !f.print.on && f.jsonSchema != "":
		return bad("-json-schema needs -p: a structured answer is for a script")
	case !f.print.on && f.inputFormat != "text":
		return bad("-input-format stream-json needs -p")
	case !f.print.on && f.format != "text":
		return bad("-output-format " + f.format + " needs -p")
	case f.partial && f.format != "stream-json":
		return bad("-include-partial-messages needs -output-format stream-json")
	}
	return 0
}

// confirmBypass asks, on a terminal, before -dangerously-skip-permissions
// turns approvals off. It is refused under a managed configuration and
// where there is no terminal to ask on. Deny rules still apply in bypass.
func confirmBypass(cfg config.Config, in io.Reader, out io.Writer) error {
	if cfg.Managed {
		return errors.New("-dangerously-skip-permissions is refused under a managed configuration")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		return errors.New("-dangerously-skip-permissions asks for confirmation on a terminal, and there is none here; -mode bypass sets the mode explicitly")
	}
	fmt.Fprint(out, "\n-dangerously-skip-permissions: the agent will run commands and change files without asking.\n"+
		"Deny rules still apply. Type yes to continue: ")
	line, err := readAnswer(in)
	if err != nil || !strings.EqualFold(strings.TrimSpace(line), "yes") {
		fmt.Fprintln(out)
		return errors.New("bypass not confirmed; nothing was started")
	}
	return nil
}
