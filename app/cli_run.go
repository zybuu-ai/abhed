package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	schema, err := schemaFlag(f.jsonSchema, workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 2
	}

	if !headless && firstRunNeeded(workspace) {
		if err := firstRun(ctx, os.Stdin, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: setup ended (%v); starting with the defaults\n", err)
		}
	}
	ff := newFlagFiles(workspace, a.trust)
	var settings []byte
	var settingsName string
	if f.settings != "" {
		if settings, settingsName, err = ff.read("-settings", f.settings); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 2
		}
	}
	cfg, err := loadSession(workspace, a.trust, !headless, settings, settingsName)
	if err != nil {
		fail(err)
	}
	registerState(cfg, workspace)
	sessionDefs, agentsSrc, err := sessionAgents(cfg, ff, f.agentsJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 2
	}
	role, err := roleFor(cfg, sessionDefs, f.agentName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 2
	}
	modelID := f.modelID
	if modelID == "" && role != nil {
		modelID = role.Model // -model, when given, is the person's own choice
	}
	if cfg, err = modelFlag(cfg, modelID); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: -model: %v\n", err)
		return 2
	}
	if err := skipPermissions(cfg, f, func(c config.Config) error { return confirmBypass(c, os.Stdin, os.Stderr) }); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 2
	}
	modeFlagGiven = f.mode != ""
	if cfg, err = applyFlags(cfg, f.mode, f.maxTurns, joinRules(f.allow, f.allowedTools), joinRules(f.deny, f.disallowedTools), f.addDirs); err != nil {
		fail(err)
	}
	if cfg, err = budgetFlag(cfg, f.maxBudget); err != nil {
		fail(err)
	}
	cfg, mcpSrc, err := mcpFlags(cfg, ff, f.mcpConfig, f.strictMCP)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 2
	}
	if role != nil {
		from := policy.Mode(orDefault(cfg.Permissions.Mode, "default"))
		if to := agent.RoleMode(from, role.PermissionMode); to != from {
			if cfg, err = cfg.Apply(config.Overrides{Mode: string(to)}); err != nil {
				fail(err)
			}
		}
	}
	if inAgentCommand() != "" {
		// Judged on the merged result, so -settings cannot do what -mode bypass may not.
		base, err := config.LoadWith(workspace, config.LoadOptions{Trust: a.trust, Quiet: true})
		if err != nil {
			fail(err)
		}
		if refuseInAgent(widened(base, cfg)) {
			return 1
		}
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
	// The session says what it found before it takes the terminal.
	probe := newEndpointProbe(provider)
	probe.start(ctx)
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
	pol.Session = &policy.Overlay{}
	must(pol.AddDeny(cfg.Permissions.Deny...))
	must(pol.AddAsk(cfg.Permissions.Ask...))
	must(pol.AddAllow(cfg.Permissions.Allow...))
	must(pol.AllowGitExtensions(cfg.Permissions.GitExtensions...))

	sb, err := startSandbox(cfg, workspace, !headless)
	if err != nil {
		fail(err)
	}
	defer func() { _ = sb.Close() }()
	// With a floor the session does not wait: a process floor is never
	// none, and a none floor is shown as such until the answer is in.
	if sb.floor == "" && sb.Tier() == sandbox.TierNone {
		fmt.Fprintf(os.Stderr, "abhed: warning: %s\n", sb.Describe())
	}
	tier := string(sb.floor)
	switch sb.floor {
	case "":
		tier = string(sb.Tier())
	case sandbox.TierNone:
		// Not known yet, so the network is not described either way; each
		// result names the tier it ran under.
		tier = ""
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
		Bash: tools.Bash{Sandbox: sb.Command,
			Isolation: sandboxconfig.Isolation(cfg, tier),
			RanUnder:  func() string { return string(sb.Tier()) }},
		Parts:   toolset.All,
		Vault:   vault,
		Warn:    warnf,
		Agents:  sessionDefs,
		Sandbox: sb,
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
		Redact: vault.Session(), Definitions: set.Agents, Background: true,
		// A subagent may run on another configured model, never an endpoint.
		Models: toolset.ModelResolver(cfg), ModelNames: toolset.OfferedModels(cfg),
		// Its memory as the session's is read, but not the session's auto memory.
		Memory: func(ws string) agent.MemoryOptions {
			o := memoryOptions(cfg, pol, ws)
			o.Auto = ""
			return o
		},
	}
	registry := toolset.Subagents(set.Registry, factory, cfg.Limits.MaxParallelSubagents)
	// ask_user, for the main conversation at a terminal only (input track).
	registry = withAsk(registry, !headless)
	registry = withAutoMemory(registry, cfg, workspace, !headless)
	if role != nil {
		// The session and the subagents it starts keep only the role's tools.
		if registry, err = agent.RoleTools(registry, role); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: -agent %s: %v\n", role.Name, err)
			return 2
		}
		factory.Tools = agent.RoleToolsFor(factory.Tools, role)
		loopCfg.Effort = agent.RoleEffort(loopCfg.Effort, role.Effort)
		if role.MaxTurns > 0 && (loopCfg.MaxTurns == 0 || role.MaxTurns < loopCfg.MaxTurns) {
			loopCfg.MaxTurns = role.MaxTurns
		}
		factory.Config.Effort, factory.Config.MaxTurns = loopCfg.Effort, loopCfg.MaxTurns
	}
	// The memory in it follows the configuration and the read rules (input track).
	loopCfg.SystemPrompt = sysPrompt.apply(cliSystemPrompt(cfg, pol, workspace, adapter, set.SkillListing, registry.Names()))
	loopCfg.SystemPrompt += roleSection(role)
	start := startPayload(cfg, f, headless, provider.Model)
	sysPrompt.record(start)
	recordRunFlags(start, cfg, mcpSrc, f.strictMCP, agentsSrc, role)
	start["web"] = toolset.WebState(cfg)
	attempts := toolset.ConfigAttempts(cfg, toolset.LocalPrincipal(inAgentCommand()))

	// The CLI uses whatever the config selects. Previously this was hardcoded
	// to memory, so a Postgres-configured deployment silently lost its CLI
	// sessions while server sessions persisted — an inconsistency the user
	// would only discover when an audit came up empty.
	store, closeStore, err := openStore(context.Background(), cfg)
	if err != nil {
		fail(err)
	}
	closeAll := sync.OnceValue(func() error {
		set.CloseEgress()
		return closeSandboxThenStore(sb, closeStore, os.Stderr)
	})
	defer func() { _ = closeAll() }()
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
		o := headlessOpts{format: f.format, partial: f.partial, verbose: f.verbose, schema: schema, start: start, fence: fenceOf(sb), attempts: attempts,
			providerName: cfg.Model.Default, provider: provider, fallback: fallback}
		prompt := f.task()
		if f.inputFormat == "stream-json" {
			o.inputs = streamInputs(os.Stdin, os.Stderr)
		} else if prompt, err = headlessTask(ctx, prompt, os.Stdin, !f.noStdin && stdinIsPipe(), os.Stderr); err != nil {
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
		return headlessExit(runOnce(ctx, store, renderer, o, adapter, registry, pol, approver, sess, loopCfg, cfg, prompt, budget, set.Extensions), closeAll)
	}
	return interactive(ctx, a, store, renderer, adapter, registry, pol, approver, sess, loopCfg, cfg, provider, workspace, budget, set.Extensions,
		interactiveStart{first: f.task(), sandbox: sb, probe: probe, set: set, recordStart: func(rec *agent.Recorder, after int64) {
			if err := recordStart(rec, resumedStart(start, after), attempts, fenceOf(sb)); err != nil {
				fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			}
		}, onOpen: func(rec *agent.Recorder) {
			if fallback != nil {
				fallback.SetRecord(recordFallback(rec))
			}
		}})
}

// webSearchLabel is doctor's and serve's line on web search: on or off, and
// that only the managed configuration turns it on.
func webSearchLabel(cfg config.Config) string {
	if cfg.WebSearch.Enabled {
		return cfg.WebSearchState() + "; the agent can reach the public internet"
	}
	return cfg.WebSearchState()
}

func webFetchLabel(cfg config.Config) string { return cfg.WebFetchState() }

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

// closeSandboxThenStore closes the sandbox while the record is still open,
// so state the fence finds planted then is recorded, and says what it found.
func closeSandboxThenStore(sb interface{ Close() error }, closeStore func(), w io.Writer) error {
	err := sb.Close()
	if err != nil {
		_, _ = fmt.Fprintf(w, "abhed: %v\n", err)
	}
	closeStore()
	return err
}

// headlessExit closes the session and fails a run that otherwise completed
// when the close reports something wrong, such as a .abhed the fence found
// planted or a workspace it could not list.
func headlessExit(code int, closeAll func() error) int {
	if err := closeAll(); err != nil && code == 0 {
		return 1
	}
	return code
}

func must(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
	tools.EndBackgroundShells(shellEndWait)
	os.Exit(1)
}

// shellEndWait bounds how long an exit waits for background shells to end.
const shellEndWait = 3 * time.Second

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
	case !f.print.on && f.noStdin:
		return bad("-no-stdin needs -p")
	case f.noStdin && f.inputFormat == "stream-json":
		return bad("-no-stdin and -input-format stream-json disagree: stream-json reads stdin")
	case !f.print.on && f.format != "text":
		return bad("-output-format " + f.format + " needs -p")
	case f.partial && f.format != "stream-json":
		return bad("-include-partial-messages needs -output-format stream-json")
	}
	return 0
}

// skipPermissions applies -dangerously-skip-permissions: bypass, once
// confirm has said yes.
func skipPermissions(cfg config.Config, f *cliFlags, confirm func(config.Config) error) error {
	if !f.skipPerms {
		return nil
	}
	if err := confirm(cfg); err != nil {
		return err
	}
	f.mode = string(policy.ModeBypass)
	return nil
}

// startPayload is what session.started records about how the CLI started,
// including the permission mode and whether bypass came from a confirmed
// -dangerously-skip-permissions.
func startPayload(cfg config.Config, f *cliFlags, headless bool, model string) map[string]any {
	return map[string]any{"surface": "cli", "headless": headless, "provider": cfg.Model.Default, "model": model,
		"mode": orDefault(cfg.Permissions.Mode, "default"), "bypass_confirmed": f.skipPerms}
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
