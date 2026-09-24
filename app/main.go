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
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/hawkeye"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/docsite"
	"github.com/zybuu-ai/abhed/internal/eval"
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/index"
	"github.com/zybuu-ai/abhed/internal/k8s"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/rag"
	"github.com/zybuu-ai/abhed/internal/remote"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/skills"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/internal/websearch"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/store"
	"golang.org/x/term"
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
		format     = fs.String("output-format", "text", "text|json")
		allow      = fs.String("allow", "", "comma-separated allow rules, e.g. 'bash(go test*)'")
		deny       = fs.String("deny", "", "comma-separated deny rules")
		showVer    = fs.Bool("version", false, "print version and exit")
		listenAddr = fs.String("addr", ":8080", "listen address for abhed serve")
	)
	fs.Usage = func() { a.usage(fs) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVer {
		fmt.Println("abhed", a.version, a.edition)
		return 0
	}

	workspace, err := resolveWorkspace(*workdir)
	if err != nil {
		fail(err)
	}

	switch fs.Arg(0) {
	case "init":
		path := filepath.Join(workspace, ".abhed", "config.json")
		if err := config.WriteDefault(path); err != nil {
			fail(err)
		}
		fmt.Printf("Wrote %s\nEdit it to point at your model endpoint, then run `abhed doctor`.\n", path)
		return 0
	case "doctor":
		return a.doctor(workspace)
	case "providers":
		return providersCmd()
	case "hawkeye":
		return hawkeyeCmd(workspace, fs.Args()[1:])
	case "migrate":
		return migrateCmd(workspace, a.migrate)
	case "resolve":
		return resolveCmd(workspace, fs.Args()[1:])
	case "acp":
		// The Agent Client Protocol over stdio, for editors that speak it.
		return acpCmd(workspace, a.version)
	case "rpc":
		// Line-delimited JSON on stdin and stdout, so a caller in any language
		// can drive Abhed as a subprocess without running a server.
		return rpcCmd(workspace)
	case "user":
		return userCmd(workspace, fs.Args()[1:])
	case "secret":
		return secretCmd(fs.Args()[1:])
	case "index":
		return buildIndexCmd(workspace)
	case "eval":
		evalFlags := flag.NewFlagSet("eval", flag.ExitOnError)
		corpus := evalFlags.String("corpus", "internal/eval/corpus", "task corpus directory")
		jsonOut := evalFlags.String("json", "", "write the full report to this path")
		_ = evalFlags.Parse(fs.Args()[1:])
		return evalCmd(workspace, *corpus, *jsonOut)
	case "serve":
		// Re-parse the remaining args so `abhed serve -addr :9000` works: Go's
		// flag package stops at the first non-flag argument.
		serveFlags := flag.NewFlagSet("serve", flag.ExitOnError)
		serveAddr := serveFlags.String("addr", *listenAddr, "listen address")
		_ = serveFlags.Parse(fs.Args()[1:])
		return a.serveCmd(workspace, *serveAddr)
	default:
		// An edition's own subcommand. Anything else is not a command at all
		// and falls through to a session, as it always has.
		if cmd, ok := a.commands[fs.Arg(0)]; ok {
			return cmd(workspace, fs.Args()[1:])
		}
	}

	return run(a, workspace, *prompt, *mode, *modelID, *maxTurns, *format, *allow, *deny, *addDirs)
}

// applyFlags lays the command line over the configuration. The managed
// configuration binds the flags exactly as it binds the SDK's Options.
func applyFlags(cfg config.Config, mode string, maxTurns int, allow, deny, addDirs string) (config.Config, error) {
	return cfg.Apply(config.Overrides{
		Mode: mode, MaxTurns: maxTurns,
		Allow: splitRules(allow), Deny: splitRules(deny), AdditionalDirs: splitRules(addDirs),
	})
}

func run(a *App, workspace, prompt, modeFlag, modelFlag string, maxTurns int, format, allowFlag, denyFlag, addDirs string) int {
	cfg, err := config.Load(workspace)
	if err != nil {
		fail(err)
	}
	if modelFlag != "" {
		cfg.Model.Default = modelFlag
	}
	if cfg, err = applyFlags(cfg, modeFlag, maxTurns, allowFlag, denyFlag, addDirs); err != nil {
		fail(err)
	}

	provider, err := cfg.Provider()
	if err != nil {
		fail(err)
	}

	adapter := buildAdapter(provider)
	sess, err := tools.NewSession(workspace)
	if err != nil {
		fail(err)
	}
	if err := grantDirs(sess, cfg, ""); err != nil {
		fail(err)
	}
	if sess.Syntax, err = tools.ParseSyntaxMode(cfg.Tools.SyntaxCheck); err != nil {
		fail(err)
	}

	pol := policy.New(policy.Mode(orDefault(cfg.Permissions.Mode, "default")))
	pol.Managed = cfg.Managed
	must(pol.AddDeny(cfg.Permissions.Deny...))
	must(pol.AddAsk(cfg.Permissions.Ask...))
	must(pol.AddAllow(cfg.Permissions.Allow...))

	sb, err := buildSandbox(cfg, workspace)
	if err != nil {
		fail(err)
	}
	if sb.Tier() == sandbox.TierNone {
		fmt.Fprintf(os.Stderr, "abhed: warning: %s\n", sb.Describe())
	}

	// Custom providers are registered before any provider is resolved, so a
	// name from configuration is usable as model.default.
	for _, err := range cfg.RegisterCustomProviders() {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
	}

	// Extensions can veto a tool call, never permit one. The hook they install
	// runs first in the policy chain so it can refuse, and is structurally
	// incapable of returning Allow.
	extHost := extension.NewHost(func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "abhed: "+format+"\n", args...)
	})
	defer extHost.Close()
	for _, err := range extHost.Load(context.Background(), cfg.ExtensionSpecs()) {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
	}
	if extHost.Len() > 0 {
		pol.Hooks = append(pol.Hooks, extHost.PolicyHook(context.Background(), "session"))
	}

	// The todo tool reports through whichever loop is currently running. The
	// holder exists because the registry is built before the loop, and a
	// package-level variable would quietly share state between sessions.
	todos := &agent.LoopHolder{}
	vault := openVault()
	registry := tools.NewRegistry(
		tools.Read{}, tools.Write{}, tools.Edit{},
		tools.Glob{}, tools.Grep{}, tools.Bash{Sandbox: sb.Command, Secrets: vault.Env, SecretNames: vaultNames(vault)},
		tools.Todo{OnUpdate: func(items []tools.TodoItem, note string) {
			todos.RecordTodos(toAgentTodos(items), note)
		}},
	)

	// Subagents share the parent's budget, so a fan-out cannot multiply spend
	// invisibly. Each spawn re-prefills its own prefix (docs P3).
	budget := agent.NewBudget(
		int64(cfg.Limits.MaxBudgetTokens),
		cfg.Limits.MaxSubagents,
		cfg.Limits.NestedSubagents,
	)

	// MCP servers extend the tool surface. Every remote tool is namespaced and
	// routes through the policy engine, since Abhed cannot know what it does.
	gateway := mcp.NewGateway()
	defer gateway.Close()
	if mcpErrs := gateway.Connect(context.Background(), mcpConfigs(cfg)); len(mcpErrs) > 0 {
		for _, e := range mcpErrs {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", e)
		}
	}
	for _, t := range gateway.Tools() {
		registry.Add(t)
	}
	// A tool an extension provides is a tool like any other: it appears in the
	// model's list, goes through the policy engine, and its call and result are
	// recorded. Providing one adds a capability, never a way around the rules.
	if extTools, toolErrs := extHost.Tools(context.Background()); true {
		for _, err := range toolErrs {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		}
		for _, t := range extTools {
			registry.Add(t)
		}
	}

	for _, t := range buildRAG(cfg) {
		registry.Add(t)
	}
	for _, t := range buildInfra(cfg) {
		registry.Add(t)
	}
	skillReg, skillListing := buildSkills(cfg)
	if skillReg.Len() > 0 {
		// The server builds an adapter and a session per request, so a
		// pipeline runner cannot be bound once here as it is in the CLI. It is
		// attached where the session is built, in the server package.
		// A skill that declares a pipeline is executed rather than described:
		// the harness runs the stages, so the gathering cannot be skipped.
		registry.Add(skills.Tool{
			R:           skillReg,
			RunPipeline: pipelineRunner(adapter, registry, sess, todos),
			Input:       lastPrompt,
		})
	}
	if t, err := buildWebSearch(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: web search disabled: %v\n", err)
	} else if t != nil {
		registry.Add(t)
	}

	// Retrieval is tier 2: an accelerator over grep, not a replacement.
	if cfg.Retrieval.Enabled {
		if ix, err := openIndex(context.Background(), cfg, workspace); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: index unavailable, falling back to grep: %v\n", err)
		} else {
			registry.Add(&index.SearchTool{Index: ix})
		}
	}

	systemPrompt := agent.BuildSystemPrompt(agent.BuildOptions{
		Profile:       "main",
		Workspace:     workspace,
		Model:         provider.Model,
		ContextWindow: provider.ContextWindow,
		MemoryFiles:   agent.DiscoverMemoryFiles(workspace),
		Skills:        skillListing,
	})

	loopCfg := agent.DefaultConfig()
	loopCfg.SystemPrompt = systemPrompt
	loopCfg.MaxTurns = cfg.Limits.MaxTurns
	loopCfg.MaxTokens = cfg.Limits.MaxTokens
	loopCfg.CompactAt = cfg.Context.CompactAt
	loopCfg.OffloadAt = cfg.Context.OffloadFraction()

	factory := &agent.SubagentFactory{
		Adapter: adapter, Tools: registry, Policy: pol,
		Approver: agent.AutoApprove{Yes: true}, // subagent tools are policed by pol
		Session:  sess, Budget: budget, Config: loopCfg, Workspace: workspace,
		Redact: openVault().Redactor(),
	}
	registry.Add(agent.Task{Spawn: factory.Spawn, Profiles: agent.Profiles})
	registry.Add(agent.Tasks{Spawn: factory.Spawn, Profiles: agent.Profiles,
		Workspace: workspace, MaxParallel: cfg.Limits.MaxParallelSubagents})

	headless := prompt != ""
	jsonOut := format == "json"

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
	renderer := ui.NewRenderer(ui.LazyStdout{}, jsonOut)

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if headless {
		return runOnce(ctx, store, renderer, jsonOut, adapter, registry, pol, approver, sess, loopCfg, cfg, prompt, todos)
	}
	return interactive(ctx, a, store, renderer, adapter, registry, pol, approver, sess, loopCfg, cfg, provider, workspace, todos, extHost)
}

// sessionBudget is the one allowance the parent loop and its subagents share.
func sessionBudget(cfg config.Config) *agent.Budget {
	return agent.NewBudget(
		int64(cfg.Limits.MaxBudgetTokens),
		cfg.Limits.MaxSubagents,
		cfg.Limits.NestedSubagents,
	)
}

func runOnce(ctx context.Context, store server.EventStore, r *ui.Renderer, jsonOut bool,
	adapter model.Adapter, registry *tools.Registry, pol *policy.Engine,
	approver agent.Approver, sess *tools.Session, cfg agent.Config,
	appCfg config.Config, prompt string, holder *agent.LoopHolder) int {

	sessionID := fmt.Sprintf("s-%d", time.Now().UnixNano())
	recordSession(ctx, store, sessionID, appCfg)
	rec := agent.NewRecorder(store, sessionID, "")
	rec.Redact = openVault().Redactor()

	events := store.Subscribe(sessionID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			if jsonOut {
				b, _ := json.Marshal(ev)
				fmt.Println(string(b))
			} else {
				r.Event(ev)
			}
		}
	}()

	loop := agent.NewLoop(adapter, registry, pol, approver, sess, rec, cfg)
	loop.Budget = sessionBudget(appCfg)
	holder.Set(loop)
	setPrompt(prompt)
	loop.Compactor = agent.NewCompactor(adapter, cfg.CompactAt)
	reason, err := loop.Run(ctx, prompt)

	store.Unsubscribe(sessionID, events)
	<-done

	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		return agent.TermError.ExitCode()
	}
	if !jsonOut {
		printUsage(r, loop.Usage())
	}
	return reason.ExitCode()
}

func interactive(ctx context.Context, a *App, store server.EventStore, r *ui.Renderer,
	adapter model.Adapter, registry *tools.Registry, pol *policy.Engine,
	approver agent.Approver, sess *tools.Session, cfg agent.Config,
	appCfg config.Config, provider config.ProviderConfig, workspace string,
	todos *agent.LoopHolder, extHost *extension.Host) int {

	s := r.Style()
	sandboxLabel := "none"
	if sb, err := buildSandbox(appCfg, workspace); err == nil {
		sandboxLabel = string(sb.Tier())
		if !appCfg.Sandbox.AllowNetwork {
			sandboxLabel += " · no network"
		}
	}
	fmt.Print(ui.Banner(s, a.version, provider.Model, workspace,
		sandboxLabel, storageLabel(appCfg)))
	fmt.Printf("\n%s\n\n", s.Dim("Type a task, or /help. Ctrl-C interrupts, Ctrl-D exits."))

	// Input is read on its own goroutine so a line typed while the agent is
	// working can steer it. Reading inline meant the prompt was simply not
	// there during a turn: the only way to correct a run that had misunderstood
	// was Ctrl-C, which discards every file it had read and every result it had
	// gathered, and then the user retypes the request.
	// Line editing: arrow keys, history, Home/End, Ctrl-A/E/U/K/W. A prompt
	// where Left prints "^[[D" instead of moving the cursor reads as broken,
	// however good the agent behind it is. Falls back to plain line reads when
	// stdin is not a terminal, since raw mode on a pipe corrupts the input.
	editor := ui.NewLineReader(ui.Prompt(s))
	defer editor.Close()
	// Raw mode turns off the terminal's own newline translation, so every
	// print in the program would otherwise staircase down the screen.
	restoreStreams := editor.Capture()
	defer restoreStreams()

	// Approval input rides the one stdin reader the editor owns. Without this
	// the approver opened a second reader on stdin, racing the editor for each
	// keystroke and waiting for a "\n" raw mode never sends. Prepare also
	// pauses the thinking indicator so its animation does not overwrite the
	// prompt — the reason the [a]ccept/[r]eject line was never visible.
	prompter := ui.NewPrompter()
	defer prompter.Close()
	if ap, ok := approver.(*ui.Approver); ok {
		ap.Prepare = func(ctx context.Context) (func() (string, bool), func()) {
			wasThinking := r.PauseThinking()
			if editor.Raw() {
				// Raw TTY: answer with a single keypress.
				keys := editor.BeginApproval()
				read := func() (string, bool) {
					select {
					case k := <-keys:
						return string(k), true
					case <-ctx.Done():
						return "", false
					}
				}
				cleanup := func() {
					editor.EndApproval()
					if wasThinking {
						r.StartThinking()
					}
				}
				return read, cleanup
			}
			// Piped stdin: the answer arrives as a line on the lines channel,
			// handed over by the steering loop's prompter.Deliver.
			read := func() (string, bool) { return prompter.Await(ctx) }
			cleanup := func() {
				if wasThinking {
					r.StartThinking()
				}
			}
			return read, cleanup
		}
	}

	lines := make(chan string)
	readErr := make(chan struct{})
	go func() {
		for {
			line, err := editor.ReadLine()
			if ui.ErrInterrupted(err) {
				// Ctrl-C abandons the line being typed; it does not end the
				// session. Ctrl-D on an empty line is what exits.
				continue
			}
			if err != nil {
				close(readErr)
				return
			}
			lines <- strings.TrimSpace(line)
		}
	}()
	turn := 0
	// Outside the turn loop: a fresh Loop is built per turn, and a budget
	// built with it would reset the allowance every time.
	turnBudget := sessionBudget(appCfg)
	// Session-level state the slash commands operate on.
	undo := agent.NewUndoLog()
	sess.Checkpoint = undo.Record
	sessionState := &cliState{
		store: store, appCfg: appCfg, undo: undo,
		workspace: sess.Root, adapter: adapter, provider: provider,
	}

	for {
		if !editor.Raw() {
			fmt.Print(ui.Prompt(s))
		}
		var line string
		select {
		case <-readErr:
			fmt.Println()
			return 0
		case <-ctx.Done():
			return 0
		case line = <-lines:
		}
		if line == "" {
			continue
		}
		// "exit" and "quit" without a slash are commands too. They were sent to
		// the model as prompts, which replied "Goodbye!" while the session
		// stayed open — the CLI ignoring the one word everyone tries first.
		if bare := strings.ToLower(strings.TrimSpace(line)); bare == "exit" || bare == "quit" {
			line = "/" + bare
		}
		if strings.HasPrefix(line, "/") {
			if quit := handleCommand(ctx, line, r, pol, sess, sessionState); quit {
				return 0
			}
			continue
		}

		turn++
		sessionID := fmt.Sprintf("s-%d-%d", time.Now().Unix(), turn)
		recordSession(ctx, store, sessionID, appCfg)
		rec := agent.NewRecorder(store, sessionID, "")
		rec.Redact = openVault().Redactor()

		events := store.Subscribe(sessionID)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for ev := range events {
				r.Event(ev)
			}
		}()

		// Each task gets its own cancellable context so Ctrl-C interrupts the
		// task without killing the session.
		taskCtx, cancelTask := context.WithCancel(ctx)
		// Read the adapter from session state rather than the value captured
		// at startup, or a /model switch is silently reverted on the next turn.
		active := sessionState.adapter
		loop := agent.NewLoop(active, registry, pol, approver, sess, rec, cfg)
		loop.Budget = turnBudget
		loop.Compactor = agent.NewCompactor(active, cfg.CompactAt)
		attachExtensionSummarizer(loop.Compactor, extHost, sessionID)
		todos.Set(loop)
		sessionState.loop = loop
		sessionState.sessionID = sessionID
		undo.BeginTurn()
		setPrompt(line)

		// Run on a goroutine so the reader stays live: anything typed now is a
		// steering message, applied at the next turn boundary rather than
		// killing the run.
		type outcome struct{ err error }
		finished := make(chan outcome, 1)
		// The indicator runs from the moment the turn starts until the first
		// output arrives. A cold local model can take thirty seconds to its
		// first token, and an unmoving prompt in that window is
		// indistinguishable from a hang.
		// The turn owns the screen: the reader stays live for steering, but
		// stops painting a prompt over the output.
		editor.Quiet(true)
		r.StartThinking()
		go func() {
			_, err := loop.Run(taskCtx, line)
			finished <- outcome{err}
		}()

		var runErr error
		var queued []string
		eof := false
	steering:
		for {
			select {
			case o := <-finished:
				// The turn is over however it ended; the indicator goes with it.
				r.StopThinking()
				runErr = o.err
				break steering
			case <-readErr:
				// End of input is not a reason to abandon the work. A piped
				// script sends every line at once and closes stdin long before
				// the agent has finished; cancelling there killed the run and
				// discarded the commands that were meant to follow it.
				readErr = nil // stop selecting on a closed channel
				eof = true
				// No more input can arrive, so a pending approval must stop
				// waiting and refuse rather than hang the turn forever.
				prompter.Close()
			case msg := <-lines:
				// A line typed while an approval is waiting is the answer to it,
				// not a steering message. Deliver it there first.
				if prompter.Deliver(msg) {
					continue
				}
				if msg == "" {
					continue
				}
				if strings.HasPrefix(msg, "/") {
					// A command typed mid-run is held, not dropped. Discarding
					// it loses what the user asked for, and running it now
					// would act on a session that is still changing under it.
					queued = append(queued, msg)
					wasOn := r.PauseThinking()
					fmt.Printf("  %s\n", s.Dim("queued "+msg+" — runs when this finishes"))
					if wasOn {
						r.StartThinking()
					}
					continue
				}
				loop.Steer(msg)
				wasOn := r.PauseThinking()
				fmt.Printf("  %s\n", s.Dim("steering — applied at the next step"))
				if wasOn {
					r.StartThinking()
				}
			}
		}
		cancelTask()
		r.StopThinking() // every exit path converges here
		editor.Quiet(false)
		sessionState.accumulate(loop.Usage())

		store.Unsubscribe(sessionID, events)
		<-done

		if runErr != nil {
			fmt.Printf("%s %s\n", s.Red("error:"), runErr)
		}
		printUsage(r, loop.Usage())
		fmt.Println()

		// Anything typed as a command while the agent worked runs now, in the
		// order it was typed.
		for _, cmd := range queued {
			fmt.Printf("%s%s\n", ui.Prompt(s), cmd)
			if quit := handleCommand(ctx, cmd, r, pol, sess, sessionState); quit {
				return 0
			}
		}
		if eof {
			fmt.Println()
			return 0
		}

		if ctx.Err() != nil {
			return 130
		}
	}
}

// cliState carries what the slash commands need across turns.
type cliState struct {
	store     server.EventStore
	appCfg    config.Config
	loop      *agent.Loop
	sessionID string
	total     agent.Usage
	undo      *agent.UndoLog
	workspace string
	adapter   model.Adapter
	provider  config.ProviderConfig
	// transcript accumulates the session for /export.
	transcript []agent.Event
}

func (c *cliState) accumulate(u agent.Usage) {
	c.total.InputTokens += u.InputTokens
	c.total.OutputTokens += u.OutputTokens
	c.total.CachedTokens += u.CachedTokens
	c.total.ColdPrefillTokens += u.ColdPrefillTokens
	c.total.Turns += u.Turns
	c.total.Compactions += u.Compactions
}

func handleCommand(ctx context.Context, line string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState) bool {
	s := r.Style()
	fields := strings.Fields(line)

	switch fields[0] {
	case "/quit", "/exit":
		return true

	case "/help":
		// One list, in internal/ui: help, tab completion and the suggestion
		// menu cannot drift apart if they read the same source.
		fmt.Println(ui.HelpText(s))

	case "/mode":
		if len(fields) < 2 {
			fmt.Printf("  current mode: %s\n", pol.Mode)
			return false
		}
		m := policy.Mode(fields[1])
		switch m {
		case policy.ModeDefault, policy.ModeAcceptEdits, policy.ModePlan, policy.ModeAuto:
			pol.Mode = m
			fmt.Printf("  mode: %s\n", m)
		default:
			fmt.Printf("  %s unknown mode %q\n", s.Red("✕"), fields[1])
		}

	case "/cost":
		u := st.total
		if u.InputTokens == 0 {
			fmt.Println(s.Dim("  no usage yet this session"))
			return false
		}
		fmt.Printf("  turns          %d\n", u.Turns)
		fmt.Printf("  tokens in      %d\n", u.InputTokens)
		fmt.Printf("  tokens out     %d\n", u.OutputTokens)
		fmt.Printf("  cached         %d (%.0f%%)\n", u.CachedTokens, u.CacheHitRate()*100)
		if savings := u.PrefillSavings(); savings > 0 {
			fmt.Printf("  prefill saving %.1fx\n", savings)
		}
		fmt.Printf("  compactions    %d\n", u.Compactions)

	case "/compact":
		if st.loop == nil {
			fmt.Println(s.Dim("  nothing to compact yet"))
			return false
		}
		info, err := st.loop.Compact(ctx)
		if err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return false
		}
		fmt.Printf("  compacted %d → %d tokens\n", info.BeforeTokens, info.AfterTokens)

	case "/sessions":
		lister, ok := st.store.(interface {
			ListSessions(context.Context, int) ([]store.SessionRecord, error)
		})
		if !ok {
			fmt.Println(s.Dim("  session history needs storage.driver = postgres"))
			return false
		}
		records, err := lister.ListSessions(ctx, 20)
		if err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return false
		}
		if len(records) == 0 {
			fmt.Println(s.Dim("  no sessions recorded"))
			return false
		}
		for _, rec := range records {
			state := "running"
			if rec.EndedAt != nil {
				state = rec.TerminalReason
			}
			fmt.Printf("  %-22s %-10s %s  %s\n", rec.ID, state,
				rec.StartedAt.Format("2006-01-02 15:04"), s.Dim(rec.User))
		}

	case "/resume":
		if len(fields) < 2 {
			fmt.Println(s.Dim("  usage: /resume <session-id>   (see /sessions)"))
			return false
		}
		events, err := st.store.Events(fields[1])
		if err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return false
		}
		if len(events) == 0 {
			fmt.Printf("  %s no events for session %s\n", s.Red("✕"), fields[1])
			return false
		}
		fmt.Printf("%s\n", s.Dim(fmt.Sprintf("  replaying %d events from %s", len(events), fields[1])))
		for _, ev := range events {
			r.Event(ev)
		}

	case "/undo":
		restored, err := st.undo.Undo()
		for _, line := range restored {
			fmt.Printf("  %s\n", line)
		}
		if err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return false
		}
		fmt.Printf("  %s\n", s.Dim(fmt.Sprintf("%d turn(s) still undoable", st.undo.Pending())))

	case "/diff":
		changed := st.undo.Changed()
		if len(changed) == 0 {
			fmt.Println(s.Dim("  no files changed this session"))
			return false
		}
		for _, path := range changed {
			rel := path
			if r, err := filepath.Rel(sess.Root, path); err == nil && !strings.HasPrefix(r, "..") {
				rel = r
			}
			before, existed, _ := st.undo.Original(path)
			after, readErr := os.ReadFile(path)
			switch {
			case !existed:
				fmt.Printf("  %s %s\n", s.Green("+"), rel)
			case readErr != nil:
				fmt.Printf("  %s %s (deleted)\n", s.Red("-"), rel)
			default:
				added, removed := lineDelta(string(before), string(after))
				fmt.Printf("  %s %s  %s %s\n", s.Yellow("~"), rel,
					s.Green(fmt.Sprintf("+%d", added)), s.Red(fmt.Sprintf("-%d", removed)))
			}
		}

	case "/clear":
		st.loop = nil
		st.total = agent.Usage{}
		st.transcript = nil
		fmt.Println(s.Dim("  context cleared; the workspace is untouched"))

	case "/memory":
		files := agent.DiscoverMemoryFiles(sess.Root)
		if len(files) == 0 {
			path := filepath.Join(sess.Root, "ABHED.md")
			fmt.Printf("  %s\n", s.Dim("no memory file yet; create "+path))
			fmt.Printf("  %s\n", s.Dim("it is re-injected on every request, so keep it short"))
			return false
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			fmt.Printf("  %s %s\n", s.Bold(f), s.Dim(fmt.Sprintf("(%d bytes)", len(data))))
			for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
				fmt.Printf("    %s\n", line)
			}
		}

	case "/model":
		if len(fields) < 2 {
			fmt.Printf("  current: %s\n", st.provider.Model)
			names := make([]string, 0, len(st.appCfg.Model.Providers))
			for name := range st.appCfg.Model.Providers {
				names = append(names, name)
			}
			sort.Strings(names)
			fmt.Printf("  configured providers: %s\n", strings.Join(names, ", "))
			return false
		}
		if _, found := st.appCfg.Model.Providers[fields[1]]; !found {
			fmt.Printf("  %s no provider %q in config\n", s.Red("✕"), fields[1])
			return false
		}
		// Resolve through ProviderNamed so the selected provider's api_key_env
		// is read into APIKey. A raw map lookup skips that step, so any
		// provider other than the default (whose key applyEnv injects) would
		// build an adapter with no credential and fail the first call with 401.
		p, resolveErr := st.appCfg.ProviderNamed(fields[1])
		if resolveErr != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), resolveErr)
			return false
		}
		next, buildErr := p.Adapter()
		if buildErr != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), buildErr)
			return false
		}
		st.appCfg.Model.Default = fields[1]
		st.provider = p
		st.adapter = next
		if st.loop != nil {
			st.loop.SetAdapter(next)
		}
		fmt.Printf("  %s\n", s.Dim("switched to "+p.Model+" — the conversation is kept"))
		fmt.Printf("  %s\n", s.Dim("(the next turn re-prefills: the new provider has not seen this prefix)"))

	case "/tree":
		// Show the session as steps, so a user can see where it went wrong
		// before deciding where to fork. Without it, /fork asks for a number
		// nobody has any way to know.
		events, err := st.store.Events(st.sessionID)
		if err != nil || len(events) == 0 {
			fmt.Println(s.Dim("  nothing recorded yet"))
			return false
		}
		forkPoints(r, events)
		fmt.Printf("  %s\n", s.Dim("/fork <step> rebuilds the conversation up to a step"))

	case "/fork":
		// Rebuild the conversation from the event log up to a point and carry
		// on from there. A wrong turn three steps back should cost three
		// steps, not the session: everything before it was still right, and
		// re-establishing it means paying for the same reading twice.
		events, err := st.store.Events(st.sessionID)
		if err != nil || len(events) == 0 {
			fmt.Println(s.Dim("  nothing to fork from yet"))
			return false
		}
		if len(fields) < 2 {
			fmt.Println(s.Dim("  /fork <step> — rebuild the conversation up to a step and continue from it"))
			forkPoints(r, events)
			return false
		}
		seq, convErr := strconv.ParseInt(fields[1], 10, 64)
		if convErr != nil {
			fmt.Printf("  %s %q is not a step number\n", s.Red("✕"), fields[1])
			return false
		}
		msgs, forkErr := agent.Fork(events, seq)
		if forkErr != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), forkErr)
			return false
		}
		if st.loop == nil {
			fmt.Println(s.Dim("  no active session to fork into"))
			return false
		}
		st.loop.Restore(msgs)
		fmt.Printf("  %s\n", s.Dim(fmt.Sprintf(
			"forked at step %d — %d messages kept; the next thing you type continues from there", seq, len(msgs))))

	case "/hawkeye":
		// The summary goes to the terminal; a path writes the full report.
		events, err := st.store.Events(st.sessionID)
		if err != nil || len(events) == 0 {
			fmt.Println(s.Dim("  nothing recorded yet"))
			return false
		}
		rep := hawkeye.Analyze(st.sessionID, events)
		fmt.Print(hawkeye.Text(rep))
		if len(fields) > 1 {
			if err := writeHawkeye(fields[1], rep); err != nil {
				fmt.Printf("  %s %v\n", s.Red("✕"), err)
				return false
			}
			fmt.Printf("\n  wrote %s\n", fields[1])
		}

	case "/export":
		// HTML by default, because a transcript that needs a parser before a
		// colleague can read it usually does not get read. `/export x.json`
		// still writes the raw events for a program.
		path := filepath.Join(sess.Root, fmt.Sprintf("abhed-session-%s.html", st.sessionID))
		if len(fields) > 1 {
			path = fields[1]
		}
		events, err := st.store.Events(st.sessionID)
		if err != nil || len(events) == 0 {
			fmt.Println(s.Dim("  no transcript to export yet"))
			return false
		}
		var data []byte
		if strings.HasSuffix(path, ".json") {
			data, err = json.MarshalIndent(events, "", "  ")
		} else {
			data = []byte(agent.ExportHTML(st.sessionID, events))
		}
		if err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return false
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return false
		}
		fmt.Printf("  wrote %d events to %s\n", len(events), path)

	case "/think":
		// Reasoning is collapsed to a word count by default because on a model
		// that reasons at length it buries the answer. This turns the full
		// text on for the session.
		r.Reasoning = !r.Reasoning
		if r.Reasoning {
			// Show the block the user just saw collapsed, not only the next
			// one. A toggle that takes effect a turn later reads as broken to
			// someone looking at a summary line right now.
			if !r.ShowLastReasoning() {
				fmt.Println(s.Dim("  reasoning shown in full from the next turn"))
			}
		} else {
			fmt.Println(s.Dim("  reasoning collapsed to a summary line"))
		}

	case "/cwd":
		fmt.Printf("  %s\n", sess.Root)

	default:
		fmt.Printf("  %s unknown command %s — try /help\n", s.Red("✕"), fields[0])
	}
	return false
}

func printUsage(r *ui.Renderer, u agent.Usage) {
	s := r.Style()
	line := fmt.Sprintf("%d turns · %d in / %d out tokens", u.Turns, u.InputTokens, u.OutputTokens)
	// Cache hit rate is a UX metric as much as a capacity one (docs P8), so it
	// is shown rather than hidden in telemetry.
	if u.InputTokens > 0 && u.CachedTokens > 0 {
		line += fmt.Sprintf(" · %.0f%% cached (%.1fx prefill)", u.CacheHitRate()*100, u.PrefillSavings())
	}
	if u.Compactions > 0 {
		line += fmt.Sprintf(" · %d compaction(s)", u.Compactions)
	}
	fmt.Printf("%s\n", s.Dim(line))
}

// serveCmd starts server mode: web console, REST API, and SSE streaming over
// the same event stream the CLI consumes.
func (a *App) serveCmd(workspace, addr string) int {
	cfg, err := a.loadConfig(workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		var ed *EditionError
		if errors.As(err, &ed) {
			return 2
		}
		return 1
	}
	provider, err := cfg.Provider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}

	sb, err := buildSandbox(cfg, workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	vault := openVault()
	bash := tools.Bash{Sandbox: sb.Command, Secrets: vault.Env, SecretNames: vaultNames(vault),
		Isolation: tools.Isolation{Tier: string(sb.Tier()), Network: cfg.Sandbox.AllowNetwork}}
	// The workbench terminal's shell runs under the same backend as the agent's commands.
	if in, ok := sb.(sandbox.Interactive); ok {
		bash.Shell, bash.Isolation.Backend = in.Shell, in.Backend()
	}
	// Terminal containers a crashed run of this server left behind.
	if sw, ok := sb.(interface{ SweepShells() }); ok {
		go sw.SweepShells()
	}
	registry := tools.NewRegistry(
		tools.Read{}, tools.Write{}, tools.Edit{},
		tools.Glob{}, tools.Grep{}, bash,
	)

	gateway := mcp.NewGateway()
	defer gateway.Close()
	gateway.Connect(context.Background(), mcpConfigs(cfg))
	for _, t := range gateway.Tools() {
		registry.Add(t)
	}

	// Identity first: a provider that cannot reach its issuer is a startup
	// finding, and the rest of the banner describes a server that will not
	// start.
	authMW, err := a.buildAuth(context.Background(), cfg, workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	fmt.Printf("auth        %s\n", authLabel(cfg, authMW))
	fmt.Printf("web search  %s\n", webSearchLabel(cfg))
	if cfg.K8s.Enabled {
		writes := "read-only"
		if cfg.K8s.AllowWrites {
			writes = "reads + writes (every write needs approval)"
		}
		fmt.Printf("kubernetes  %s\n", writes)
		// Resolve the kubeconfig now: a misconfigured cluster should be a
		// startup finding, not a surprise mid-task.
		if c, err := k8s.Open(k8s.Config{Kubeconfig: cfg.K8s.Kubeconfig,
			Context: cfg.K8s.Context, Namespace: cfg.K8s.Namespace,
			Token: os.Getenv("ABHED_K8S_TOKEN")}); err != nil {
			fmt.Printf("            UNAVAILABLE — %v\n", err)
		} else {
			fmt.Printf("            context %s · namespace %s\n", c.Name, c.Namespace)
		}
	}
	// Note the absence of a len(Hosts) > 0 condition. A deployment that enables
	// SSH with no hosts listed still gets the tools, because ssh_connect is how
	// a host gets declared in the first place — requiring one in config to use
	// the tool that adds them was the bug.
	if cfg.SSH.Enabled {
		names := make([]string, 0, len(cfg.SSH.Hosts))
		for _, h := range cfg.SSH.Hosts {
			label := h.Name
			if h.InsecureSkipHostKeyCheck {
				label += " ⚠ no host key check"
			}
			names = append(names, label)
		}
		fmt.Printf("ssh hosts   %s\n", strings.Join(names, ", "))
	}
	if n := len(cfg.RAG.Corpora); n > 0 {
		names := make([]string, 0, n)
		for _, c := range cfg.RAG.Corpora {
			if c.Enabled {
				names = append(names, c.Name)
			}
		}
		if len(names) > 0 {
			fmt.Printf("rag corpora %s\n", strings.Join(names, ", "))
		}
	}
	fmt.Printf("storage     %s\n", storageLabel(cfg))
	if cfg.Storage.Driver == "postgres" {
		st, closeFn, err := openStore(context.Background(), cfg)
		if err != nil {
			fmt.Printf("            UNAVAILABLE — %v\n", err)
		} else {
			if pg, ok := st.(*store.Postgres); ok {
				printStoreStatus(pg)
			}
			closeFn()
		}
	}
	for _, t := range buildRAG(cfg) {
		registry.Add(t)
	}
	for _, t := range buildInfra(cfg) {
		registry.Add(t)
	}
	skillReg, skillListing := buildSkills(cfg)
	if skillReg.Len() > 0 {
		// The server builds an adapter and a session per request, so a
		// pipeline runner cannot be bound once here as it is in the CLI.
		// Skills that declare one fall back to their instructions on this
		// path until the server attaches a runner where it builds a session.
		registry.Add(skills.Tool{R: skillReg})
	}
	if t, err := buildWebSearch(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: web search disabled: %v\n", err)
	} else if t != nil {
		registry.Add(t)
	}
	// Kept rather than discarded: the settings surface can trigger a reindex,
	// which needs the same Index the search tool is reading.
	var searchIndex *index.Index
	if cfg.Retrieval.Enabled {
		if ix, err := openIndex(context.Background(), cfg, workspace); err == nil {
			registry.Add(&index.SearchTool{Index: ix})
			searchIndex = ix
		}
	}

	eventStore, closeStore, err := openStore(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	defer closeStore()

	// Taps on the event store: an exporter being slow or absent costs spans,
	// never turns. Several are fanned in; each is unaware of the others.
	var taps []func(agent.Event)
	for _, build := range a.taps {
		tap, stop, err := build(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
		if stop != nil {
			defer stop()
		}
		if tap != nil {
			taps = append(taps, tap)
		}
	}

	opts := server.Options{
		Addr:         addr,
		Workspace:    workspace,
		HomeURL:      cfg.Server.HomeURL,
		EventTap:     fanIn(taps),
		Config:       cfg,
		Adapter:      buildAdapter(provider),
		Registry:     registry,
		Redact:       openVault().Redactor(),
		SkillListing: skillListing,
		SkillDirs:    skillDirs(cfg),
		Store:        eventStore,
		Auth:         authMW,
		// The live objects behind the settings surface. Passing the registries
		// rather than only their rendered output is what lets a change reach
		// the next session without a restart.
		SkillRegistry: skillReg,
		SkillRoots:    skillRoots(cfg),
		Gateway:       gateway,
		Index:         searchIndex,
		IndexOptions:  indexOptions(cfg),
		DrainTimeout:  time.Duration(cfg.Server.DrainSeconds) * time.Second,
	}
	for _, h := range a.serverOpts {
		if err := h(cfg, &opts); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 2
		}
	}
	srv := server.New(opts)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for _, h := range a.serveHooks {
		stopHook, err := h(ctx, srv, cfg)
		if err != nil {
			// A hook failing is a startup refusal, like a bad config: nothing
			// has been served yet and the operator is looking at the terminal.
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 2
		}
		if stopHook != nil {
			defer stopHook()
		}
	}

	bs := ui.NewStyle(os.Stdout)
	fmt.Printf("%s %s %s  %s\n", bs.Cyan(ui.Glyph),
		bs.Bold("ABHED"), bs.Dim(a.version), browsableURL(addr))
	fmt.Printf("  workspace %s\n  model     %s\n  sandbox   %s\n  storage   %s\n",
		workspace, provider.Model, sb.Tier(), storageLabel(cfg))
	fmt.Printf("  auth      %s\n", authLabel(cfg, authMW))
	switch {
	case docsite.Available():
		fmt.Printf("  docs      %s/docs\n", browsableURL(addr))
	case docsite.PublicURL != "":
		// Not embedded in this build; say where the same pages are, once,
		// here — never from a page, which must stay usable with no route out.
		fmt.Printf("  docs      %s (not embedded in this build)\n", docsite.PublicURL)
	}

	if err := srv.ListenAndServe(ctx); err != nil && err.Error() != "http: Server closed" {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	return 0
}

// fanIn joins taps into one, or none, so the server never wraps its store
// around a function that does nothing.
func fanIn(taps []func(agent.Event)) func(agent.Event) {
	switch len(taps) {
	case 0:
		return nil
	case 1:
		return taps[0]
	}
	return func(ev agent.Event) {
		for _, t := range taps {
			t(ev)
		}
	}
}

// evalAllow lets corpora that compile and test code do so unattended.
var evalAllow = []string{"bash(go *)", "bash(npm *)", "bash(python *)", "bash(cat *)", "bash(ls*)"}

// evalCmd runs the evaluation corpus against the configured model.
//
// Per docs P1 the harness is the dominant variable in agent success, so this is
// how a harness change is judged. Per P10 the report carries behavioural flags
// alongside the score, because identical pass rates hide different behaviour.
func evalCmd(workspace, corpusDir, jsonPath string) int {
	cfg, err := config.Load(workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	// An eval runs unattended in auto mode with build-tool allow rules, which
	// a managed configuration may forbid like any other override.
	if _, err := cfg.Apply(config.Overrides{Mode: "auto", Allow: evalAllow}); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: eval: %v\n", err)
		return 1
	}
	provider, err := cfg.Provider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}

	tasks, err := eval.LoadTasks(corpusDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	fmt.Printf("running %d tasks against %s\n\n", len(tasks), provider.Model)

	adapter := buildAdapter(provider)
	evalSkills, evalSkillListing := buildSkills(cfg)
	workRoot, err := os.MkdirTemp("", "abhed-eval-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(workRoot) }()

	runner := func(ctx context.Context, ws string, task eval.Task) ([]agent.Event, eval.Result, error) {
		sb, err := buildSandbox(cfg, ws)
		if err != nil {
			return nil, eval.Result{}, err
		}
		sess, err := tools.NewSession(ws)
		if err != nil {
			return nil, eval.Result{}, err
		}
		if sess.Syntax, err = tools.ParseSyntaxMode(cfg.Tools.SyntaxCheck); err != nil {
			return nil, eval.Result{}, err
		}
		vault := openVault()
		registry := tools.NewRegistry(
			tools.Read{}, tools.Write{}, tools.Edit{},
			tools.Glob{}, tools.Grep{}, tools.Bash{Sandbox: sb.Command, Secrets: vault.Env, SecretNames: vaultNames(vault)},
		)
		// Skills and web search are part of the agent under test, not extras.
		// Without them a corpus that exercises a retrieval skill measures an
		// agent that never had one — it would score zero and say nothing about
		// the harness.
		if evalSkills.Len() > 0 {
			registry.Add(skills.Tool{R: evalSkills})
		}
		if t, err := buildWebSearch(cfg); err == nil && t != nil {
			registry.Add(t)
		}

		pol := policy.New(policy.ModeAuto)
		pol.Managed = cfg.Managed
		must(pol.AddDeny(cfg.Permissions.Deny...))
		// The operator's own allow rules apply, so an eval run is governed the
		// same way a real session is. The build-tool defaults stay for corpora
		// that compile and test code.
		must(pol.AddAllow(cfg.Permissions.Allow...))
		must(pol.AddAllow(evalAllow...))

		store := agent.NewMemStore()
		sessionID := "eval-" + task.ID
		rec := agent.NewRecorder(store, sessionID, "")
		rec.Redact = openVault().Redactor()

		loopCfg := agent.DefaultConfig()
		loopCfg.SystemPrompt = agent.BuildSystemPrompt(agent.BuildOptions{
			Profile: "main", Workspace: ws,
			Model: provider.Model, ContextWindow: provider.ContextWindow,
			Skills: evalSkillListing,
		})
		if task.MaxTurns > 0 {
			loopCfg.MaxTurns = task.MaxTurns
		} else {
			loopCfg.MaxTurns = 30
		}

		loop := agent.NewLoop(adapter, registry, pol, agent.AutoApprove{Yes: true}, sess, rec, loopCfg)
		loop.Compactor = agent.NewCompactor(adapter, loopCfg.CompactAt)

		runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()

		reason, runErr := loop.Run(runCtx, task.Prompt)
		usage := loop.Usage()
		events, _ := store.Events(sessionID)

		return events, eval.Result{
			Turns: usage.Turns, TokensIn: usage.InputTokens, TokensOut: usage.OutputTokens,
			CacheHitRate: usage.CacheHitRate(), Compactions: usage.Compactions,
			Terminal: string(reason),
		}, runErr
	}

	results, err := eval.Run(context.Background(), tasks, workRoot, runner)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}

	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
		}
		fmt.Printf("  %-4s %-28s %2d turns  %6d tok\n", status, r.TaskID, r.Turns, r.TokensIn)
		for _, f := range r.Flags {
			marker := "flag"
			if f.Blocking {
				marker = "BLOCK"
			}
			fmt.Printf("       %s %s: %s\n", marker, f.Kind, f.Detail)
		}
		for _, f := range r.Failures {
			fmt.Printf("       %s\n", f)
		}
	}

	summary := eval.Summarize(results, provider.Model)
	fmt.Printf("\n%s", summary.Render())

	if jsonPath != "" {
		data, _ := json.MarshalIndent(summary, "", "  ")
		if err := os.WriteFile(jsonPath, append(data, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: writing %s: %v\n", jsonPath, err)
			return 1
		}
		fmt.Printf("\nreport written to %s\n", jsonPath)
	}

	// A non-zero exit lets CI gate on the eval, which is the point.
	if summary.Passed < summary.Tasks {
		return 1
	}
	return 0
}

// splitPositional pulls the first non-flag argument out of a list, returning
// the remaining flags and that value. Needed because flag.Parse treats the
// first bare word as the end of the flags.
func splitPositional(args []string) (flags []string, positional string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if positional == "" && !strings.HasPrefix(a, "-") {
			// Not a flag value: the preceding token, if a flag, used "=" or
			// is boolean. Abhed's user flags all take values, so a bare word
			// following "-email" belongs to it.
			if i > 0 && strings.HasPrefix(args[i-1], "-") &&
				!strings.Contains(args[i-1], "=") {
				flags = append(flags, a)
				continue
			}
			positional = a
			continue
		}
		flags = append(flags, a)
	}
	return flags, positional
}

// userCmd manages local accounts: abhed user add | list | passwd | remove.
func userCmd(workspace string, args []string) int {
	cfg, err := config.Load(workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	if cfg.Auth.Mode != "local" {
		fmt.Fprintf(os.Stderr,
			"abhed: auth.mode is %q, so Abhed does not hold accounts.\n"+
				"Set \"auth\": {\"mode\": \"local\"} to manage users here.\n",
			orDefault(cfg.Auth.Mode, "none"))
		return 1
	}

	us, err := userStore(cfg, workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	if fs, isFile := us.(*auth.FileUserStore); isFile {
		fmt.Fprintf(os.Stderr, "abhed: accounts in %s "+
			"(set storage.driver to postgres for a multi-node deployment)\n", fs.Path())
	}
	la := auth.NewLocalAuth(us, 0, cfg.Auth.CookieSecure)
	ctx := context.Background()

	action := "list"
	if len(args) > 0 {
		action = args[0]
	}

	switch action {
	case "add":
		fs := flag.NewFlagSet("user add", flag.ExitOnError)
		email := fs.String("email", "", "email address")
		name := fs.String("name", "", "display name")
		tenant := fs.String("tenant", "", "tenant (defaults to storage.tenant)")
		groups := fs.String("groups", "", "comma-separated groups")
		admin := fs.Bool("admin", false, "grant administrator rights (settings, users, invites)")
		pass := fs.String("password", "", "password (generated if omitted)")

		// Go's flag package stops at the first non-flag argument, so parsing
		// "add demo -email x" would silently discard every flag after the
		// username. Lift the positional out first, then parse the rest.
		rest, username := splitPositional(args[1:])
		if username == "" {
			fmt.Fprintln(os.Stderr, "usage: abhed user add <username> [-email ...] [-name ...]")
			return 2
		}
		_ = fs.Parse(rest) // the set exits on a bad flag; nothing is left to check

		password := *pass
		if password == "" {
			password = generatePassword()
			fmt.Printf("generated password: %s\n", password)
			fmt.Println("  (change it after first sign-in)")
		}

		u := auth.User{
			Username: username, Email: *email, Name: *name,
			Tenant: orDefault(*tenant, orDefault(cfg.Storage.Tenant, "default")),
		}
		if *groups != "" {
			u.Groups = splitRules(*groups)
		}
		// The admin group is what gates settings, users and invites. A
		// deployment whose first account is not an admin cannot reach its own
		// settings page, so this is offered as a flag rather than something to
		// discover from a config file.
		if *admin {
			g := cfg.Auth.AdminGroup
			if g == "" {
				g = server.DefaultAdminGroup
			}
			if !slices.Contains(u.Groups, g) {
				u.Groups = append(u.Groups, g)
			}
		}
		if err := la.CreateUser(ctx, u, password); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
		fmt.Printf("created %s (tenant %s)\n", username, u.Tenant)
		if *admin {
			fmt.Println("  administrator — can manage settings and users")
		}

	case "list":
		users, err := us.List(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
		if len(users) == 0 {
			fmt.Println("no accounts yet — create one with: abhed user add <username>")
			return 0
		}
		sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
		fmt.Printf("%-20s %-28s %-12s %s\n", "USERNAME", "EMAIL", "TENANT", "GROUPS")
		for _, u := range users {
			fmt.Printf("%-20s %-28s %-12s %s\n",
				u.Username, orDefault(u.Email, "—"), u.Tenant, strings.Join(u.Groups, ","))
		}

	case "passwd":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: abhed user passwd <username>")
			return 2
		}
		u, err := us.Get(ctx, args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
		password := generatePassword()
		if err := la.CreateUserOrReset(ctx, u, password); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
		fmt.Printf("new password for %s: %s\n", u.Username, password)

	case "remove", "rm":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: abhed user remove <username>")
			return 2
		}
		if err := us.Delete(ctx, args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
		fmt.Printf("removed %s\n", args[1])

	case "import":
		// Switching storage.driver from memory/file to postgres leaves every
		// existing account behind in the file, with no error and no hint —
		// the accounts simply are not there any more. This moves them.
		if cfg.Storage.Driver != "postgres" {
			fmt.Fprintln(os.Stderr,
				"abhed: import copies accounts INTO postgres; set storage.driver first")
			return 1
		}
		src, err := auth.NewFileUserStore(usersFile(cfg, workspace))
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
		accounts, err := src.List(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: read %s: %v\n", src.Path(), err)
			return 1
		}
		if len(accounts) == 0 {
			fmt.Printf("no accounts in %s\n", src.Path())
			return 0
		}
		moved, skipped := 0, 0
		for _, u := range accounts {
			// Never overwrite an account that already exists in the target:
			// a re-run of import must not clobber a password changed since.
			if existing, _ := us.Get(ctx, u.Username); existing != nil {
				fmt.Printf("  skip   %s (already in postgres)\n", u.Username)
				skipped++
				continue
			}
			if err := us.Put(ctx, u); err != nil {
				fmt.Fprintf(os.Stderr, "abhed: import %s: %v\n", u.Username, err)
				return 1
			}
			fmt.Printf("  import %s\n", u.Username)
			moved++
		}
		fmt.Printf("%d imported, %d already present\n", moved, skipped)
		if moved > 0 {
			// Left in place deliberately: it is the only copy of those hashes
			// until the operator is satisfied the move worked.
			fmt.Printf("%s is unchanged — delete it once you have signed in\n", src.Path())
		}

	default:
		fmt.Fprintln(os.Stderr, "usage: abhed user [add|list|passwd|remove|import]")
		return 2
	}
	return 0
}

// generatePassword produces a readable but strong initial password, so an
// operator can hand it over without inventing one that turns out to be weak.
func generatePassword() string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		panic("abhed: system random source unavailable: " + err.Error())
	}
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = alphabet[int(v)%len(alphabet)]
	}
	return string(out)
}

// browsableURL turns a listen address into one that can be pasted into a
// browser. The banner used to print "http://localhost" + addr, which is right
// for the default ":8080" and nonsense for anything else: binding
// "127.0.0.1:8080" (as the container does) announced
// "http://localhost127.0.0.1:8080".
//
// A wildcard bind has no single correct URL, so it resolves to localhost, which
// is the one address the person reading the banner is certainly able to reach.
func browsableURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// buildAuth constructs the identity layer from the registered builders.
//
// Local accounts and an identity provider are independent capabilities, not
// alternatives. A team may well want both — passwords for contractors who are
// not in the corporate directory, an identity provider for staff — so "local"
// enables the password path and a configured provider adds the button beside
// it. Proxy mode is neither: it trusts headers, which is only safe behind a
// trusted proxy, and there is nothing to build for it.
func (a *App) buildAuth(ctx context.Context, cfg config.Config, workspace string) (*auth.Middleware, error) {
	// Only what must answer before a caller is signed in. Anything else added
	// here is an unauthenticated endpoint on a public port, so the list is
	// kept short deliberately; each provider adds the paths it owns.
	public := server.PublicPaths()
	mw := &auth.Middleware{}

	var users auth.UserStore
	for _, mode := range authModes(cfg) {
		if mode == "proxy" {
			mw.TrustHeaders = true
			continue
		}
		build := a.auth[mode]
		if build == nil {
			return nil, &EditionError{Key: fmt.Sprintf("auth.mode %q", mode), Feature: mode, Edition: a.edition}
		}
		if mode == "local" {
			// Users live in the same Postgres as the event store when one is
			// configured, so accounts survive a restart.
			var err error
			if users, err = userStore(cfg, workspace); err != nil {
				return nil, err
			}
		}
		p, v, err := build(ctx, cfg, workspace, users)
		if err != nil {
			return nil, err
		}
		if p != nil {
			mw.Providers = append(mw.Providers, p)
			public = append(public, p.PublicPaths()...)
		}
		if v != nil {
			mw.Verifier = v
		}
	}
	mw.PublicPaths = public
	return mw, nil
}

// authModes lists the mechanisms a config asks for, in the order they are
// consulted. A local deployment that also names a provider gets both; without
// a client id there is nothing to redirect to, so it stays local-only.
func authModes(cfg config.Config) []string {
	switch cfg.Auth.Mode {
	case "local":
		if cfg.Auth.Provider != "" && cfg.Auth.ClientID != "" {
			return []string{"local", "oidc"}
		}
		return []string{"local"}
	case "oidc", "proxy":
		return []string{cfg.Auth.Mode}
	}
	return nil
}

// authLabel describes the identity layer for the banner and for doctor.
func authLabel(cfg config.Config, mw *auth.Middleware) string {
	var providers []string
	if mw != nil {
		for _, p := range mw.Providers {
			if _, label := p.SignIn(); label != "" {
				providers = append(providers, label)
			}
		}
	}
	switch cfg.Auth.Mode {
	case "local":
		s := "local accounts (username and password)"
		if len(providers) > 0 {
			s += " + " + strings.Join(providers, " + ")
		}
		return s
	case "oidc":
		if len(providers) == 0 {
			return "oidc (bearer tokens; no browser sign-in configured)"
		}
		return "oidc (" + strings.Join(providers, ", ") + ")"
	case "proxy":
		return "proxy headers (only safe behind a trusted proxy)"
	default:
		return "none (single-tenant, no authentication)"
	}
}

// recordSession creates the durable session row that events reference.
// A no-op on the memory store, which has no session table.
func recordSession(ctx context.Context, st server.EventStore, id string, cfg config.Config) {
	rec, ok := st.(interface {
		CreateSession(context.Context, store.SessionRecord) error
	})
	if !ok {
		return
	}
	user := os.Getenv("USER")
	if user == "" {
		user = "local"
	}
	provider, _ := cfg.Provider()
	tenant := cfg.Storage.Tenant
	if tenant == "" {
		tenant = "default"
	}
	if err := rec.CreateSession(ctx, store.SessionRecord{
		ID: id, Tenant: tenant, User: user,
		Workspace: mustCwd(), Model: provider.Model,
		Mode:      orDefault(cfg.Permissions.Mode, "default"),
		StartedAt: time.Now().UTC(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: could not persist session: %v\n", err)
	}
}

// lineDelta counts added and removed lines between two versions, for /diff.
func lineDelta(before, after string) (added, removed int) {
	b := strings.Split(before, "\n")
	a := strings.Split(after, "\n")
	counts := map[string]int{}
	for _, line := range b {
		counts[line]++
	}
	for _, line := range a {
		if counts[line] > 0 {
			counts[line]--
		} else {
			added++
		}
	}
	for _, n := range counts {
		removed += n
	}
	return added, removed
}

func mustCwd() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// openStore selects the event store. Memory is fine for a CLI session; audit
// and replay across restarts need Postgres.
func openStore(ctx context.Context, cfg config.Config) (server.EventStore, func(), error) {
	if cfg.Storage.Driver != "postgres" {
		return agent.NewMemStore(), func() {}, nil
	}
	pg, err := store.Open(ctx, storeConfig(cfg))
	if err != nil {
		return nil, nil, fmt.Errorf("open event store: %w", err)
	}
	return pg, pg.Close, nil
}

func storageLabel(cfg config.Config) string {
	if cfg.Storage.Driver == "postgres" {
		// Only what configuration says. Whether the record is protected is a
		// fact about the connection, and doctor reports it after asking.
		label := "postgres (durable, tenant=" + orDefault(cfg.Storage.Tenant, "default")
		if cfg.Storage.SingleRole {
			label += "; single_role: the server's role can alter the record"
		}
		return label + ")"
	}
	return "memory (sessions do not survive restart)"
}

// indexOptions mirrors what openIndex uses, so a reindex triggered from the
// settings surface rebuilds on the same terms as the startup build rather than
// quietly dropping the vector tier.
func indexOptions(cfg config.Config) index.BuildOptions {
	opts := index.DefaultBuildOptions()
	opts.Embed = cfg.Retrieval.Embed && cfg.Retrieval.EmbedBaseURL != ""
	return opts
}

// openIndex builds the retrieval index for this workspace.
func openIndex(ctx context.Context, cfg config.Config, workspace string) (*index.Index, error) {
	ix := index.New(workspace)
	opts := index.DefaultBuildOptions()

	if cfg.Retrieval.Embed && cfg.Retrieval.EmbedBaseURL != "" {
		provider, _ := cfg.Provider()
		ix = ix.WithEmbedder(index.NewOpenAIEmbedder(
			cfg.Retrieval.EmbedBaseURL, provider.APIKey,
			cfg.Retrieval.EmbedModel, cfg.Retrieval.EmbedDims))
		opts.Embed = true
	}
	if err := ix.Build(ctx, opts); err != nil {
		return nil, err
	}
	return ix, nil
}

// buildIndexCmd implements `abhed index`, so a large repo can be indexed once
// rather than on every session start.
func buildIndexCmd(workspace string) int {
	cfg, err := config.Load(workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	fmt.Printf("indexing %s...\n", workspace)
	start := time.Now()

	ix, err := openIndex(context.Background(), cfg, workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	docs, terms, vectors, _ := ix.Stats()
	fmt.Printf("  %d chunks · %d terms · %d vectors · %s\n",
		docs, terms, vectors, time.Since(start).Round(time.Millisecond))
	if vectors == 0 {
		fmt.Printf("  (symbol + BM25 tiers only; set retrieval.embed to add the vector tier)\n")
	}
	return 0
}

func mcpConfigs(cfg config.Config) []mcp.ServerConfig {
	out := make([]mcp.ServerConfig, 0, len(cfg.MCP.Servers))
	for _, s := range cfg.MCP.Servers {
		out = append(out, mcp.ServerConfig{
			Name: s.Name, Command: s.Command, Args: s.Args, Env: s.Env,
			URL: s.URL, Headers: s.Headers, HeadersEnv: s.HeadersEnv,
			Enabled: s.Enabled, AllowTools: s.AllowTools, Digest: s.Digest,
		})
	}
	return out
}

// userStore returns durable account storage when Postgres is configured, and
// in-memory otherwise. In-memory is fine for a pilot but says so at startup:
// accounts vanishing on restart should never be a surprise.
func userStore(cfg config.Config, workspace string) (auth.UserStore, error) {
	if cfg.Storage.Driver == "postgres" {
		pg, err := store.Open(context.Background(), storeConfig(cfg))
		if err != nil {
			return nil, fmt.Errorf("open user store: %w", err)
		}
		return pg, nil
	}
	// No Postgres: keep accounts in a file beside the workspace config, so
	// `abhed user add` and `abhed serve` see the same accounts. An in-memory
	// store here silently discarded every account the CLI created.
	return auth.NewFileUserStore(usersFile(cfg, workspace))
}

// usersFile is where local accounts live: the configured path, else beside
// the workspace config.
func usersFile(cfg config.Config, workspace string) string {
	if cfg.Auth.UsersFile != "" {
		return cfg.Auth.UsersFile
	}
	return filepath.Join(workspace, ".abhed", "users.json")
}

func storeConfig(cfg config.Config) store.Config {
	sc := store.DefaultConfig(cfg.Storage.DSN)
	sc.SingleRole = cfg.Storage.SingleRole
	if cfg.Storage.Tenant != "" {
		sc.Tenant = cfg.Storage.Tenant
	}
	if cfg.Storage.MaxConns > 0 {
		sc.MaxConns = int32(cfg.Storage.MaxConns)
	}
	return sc
}

// buildWebSearch constructs the web search tool when enabled. Returns nil, nil
// when the operator has left it off, which is the default.
func buildWebSearch(cfg config.Config) (tools.Tool, error) {
	if !cfg.WebSearch.Enabled {
		return nil, nil
	}
	key := cfg.WebSearch.APIKey
	if key == "" && cfg.WebSearch.APIKeyEnv != "" {
		key = os.Getenv(cfg.WebSearch.APIKeyEnv)
	}
	p, err := websearch.New(websearch.Config{
		Provider:   cfg.WebSearch.Provider,
		APIKey:     key,
		BaseURL:    cfg.WebSearch.BaseURL,
		MaxResults: cfg.WebSearch.MaxResults,
	})
	if err != nil {
		return nil, err
	}
	return &websearch.Tool{Provider: p, Limit: cfg.WebSearch.MaxResults}, nil
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

// buildSandbox selects an execution backend meeting the configured minimum
// tier. Select never silently downgrades, so a failure here is a real
// configuration problem the operator must see.
func buildSandbox(cfg config.Config, workspace string) (sandbox.Sandbox, error) {
	return sandboxconfig.Build(cfg, workspace)
}

// grantDirs widens the session's reachable set from config and the --add-dir
// flag. Both are operator input: nothing the model says reaches this, which is
// the whole point of the boundary.
func grantDirs(sess *tools.Session, cfg config.Config, flagDirs string) error {
	dirs := append([]string{}, cfg.AdditionalDirs...)
	dirs = append(dirs, splitRules(flagDirs)...)
	// Skill directories are reachable by construction: a skill's instructions
	// routinely say "run the script in scripts/run.sh", and denying the read
	// of a file the operator installed deliberately sends the agent into a
	// loop it cannot escape. These are operator-configured paths, not
	// workspace content, so this widens nothing the operator did not choose.
	dirs = append(dirs, skillDirs(cfg)...)
	for _, d := range dirs {
		if err := sess.AddRoot(d); err != nil {
			return fmt.Errorf("--add-dir: %w", err)
		}
	}
	return nil
}

// buildSkills loads the configured skill directories and returns the registry
// plus its prompt listing. Errors are reported and survivable: one malformed
// SKILL.md should not stop the agent starting.
// skillDirs returns each loaded skill's own directory, for filesystem access.
func skillDirs(cfg config.Config) []string {
	if cfg.Skills.Disabled {
		return nil
	}
	reg, _ := buildSkills(cfg)
	var out []string
	for _, s := range reg.All() {
		if s.Dir != "" {
			out = append(out, s.Dir)
		}
	}
	return out
}

// skillRoots is where skills are looked FOR, as distinct from skillDirs, which
// returns each loaded skill's own directory so its assets can be read.
//
// The two were easy to confuse and the confusion was silent: reloading from
// skillDirs scans inside individual skills and finds nothing, so a reload
// reported zero skills loaded while eleven were live.
func skillRoots(cfg config.Config) []string {
	if cfg.Skills.Disabled {
		return nil
	}
	if dirs := cfg.Skills.Dirs; len(dirs) > 0 {
		return dirs
	}
	return []string{"~/.abhed/skills"}
}

func buildSkills(cfg config.Config) (*skills.Registry, string) {
	if cfg.Skills.Disabled {
		return skills.NewRegistry(), ""
	}
	dirs := skillRoots(cfg)
	reg, errs := skills.Load(dirs)
	for _, err := range errs {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
	}
	return reg, reg.Listing()
}

// buildInfra constructs the cluster and remote-host tools. Both are off by
// default and both report why they are unavailable rather than silently
// registering nothing.
func buildInfra(cfg config.Config) []tools.Tool {
	var out []tools.Tool

	if cfg.K8s.Enabled {
		mgr := k8s.NewManager(k8s.Config{
			Kubeconfig: cfg.K8s.Kubeconfig,
			Context:    cfg.K8s.Context,
			Namespace:  cfg.K8s.Namespace,
			// From the environment only: a token in a config file sits in a
			// directory the agent itself can read.
			Token: os.Getenv("ABHED_K8S_TOKEN"),
		})
		out = append(out, k8s.GetTool{M: mgr}, k8s.LoginTool{M: mgr})
		if cfg.K8s.AllowWrites {
			out = append(out, k8s.ApplyTool{M: mgr})
		}
	}

	// No len(Hosts) > 0 condition: ssh_connect is how a host gets declared in
	// the first place, so requiring one in config to reach the tool that adds
	// them was the bug — a user with a VM and a key had no way in.
	if cfg.SSH.Enabled {
		hosts := make([]remote.HostConfig, 0, len(cfg.SSH.Hosts))
		for _, h := range cfg.SSH.Hosts {
			hosts = append(hosts, remote.HostConfig{
				Name: h.Name, Addr: h.Addr, User: h.User,
				IdentityFile: h.IdentityFile, PasswordEnv: h.PasswordEnv,
				KnownHostsFile:           h.KnownHostsFile,
				InsecureSkipHostKeyCheck: h.InsecureSkipHostKeyCheck,
			})
			if h.InsecureSkipHostKeyCheck {
				fmt.Fprintf(os.Stderr, "abhed: ssh host %q skips host key "+
					"verification — it cannot detect a machine-in-the-middle\n", h.Name)
			}
		}
		reg, errs := remote.NewRegistry(hosts)
		for _, err := range errs {
			fmt.Fprintf(os.Stderr, "abhed: ssh: %v\n", err)
		}
		out = append(out, remote.Tool{R: reg}, remote.ConnectTool{R: reg})
	}
	return out
}

// buildRAG constructs a tool per enabled corpus. A corpus that cannot be
// configured is reported and skipped rather than failing startup: one broken
// endpoint should not take the whole agent down.
func buildRAG(cfg config.Config) []tools.Tool {
	var out []tools.Tool
	for _, c := range cfg.RAG.Corpora {
		if !c.Enabled {
			continue
		}
		headers := map[string]string{}
		for k, v := range c.Headers {
			headers[k] = v
		}
		for k, envVar := range c.HeadersEnv {
			if v := os.Getenv(envVar); v != "" {
				headers[k] = v
			} else {
				fmt.Fprintf(os.Stderr,
					"abhed: rag corpus %q needs %s in the environment; skipping\n",
					c.Name, envVar)
				headers = nil
				break
			}
		}
		if headers == nil {
			continue
		}
		r, err := rag.New(rag.Config{
			Name: c.Name, Description: c.Description, URL: c.URL, Method: c.Method,
			Headers: headers, QueryField: c.QueryField, QueryParam: c.QueryParam,
			TopKField: c.TopKField, TopK: c.TopK, Body: c.Body,
			ResultsPath: c.ResultsPath, TextField: c.TextField,
			SourceField: c.SourceField, TitleField: c.TitleField, ScoreField: c.ScoreField,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: rag corpus %q: %v\n", c.Name, err)
			continue
		}
		out = append(out, &rag.Tool{R: r})
	}
	return out
}

// buildAdapter constructs the configured provider.
//
// Every provider is reached through the registry in internal/model, so adding
// one is a new file with an init rather than another branch here. A build
// failure is fatal by design: a mistyped provider type or an unsupported
// sampling parameter is a configuration error, and discovering it now beats
// discovering it on the first model call of a long session.
func buildAdapter(p config.ProviderConfig) model.Adapter {
	a, err := p.Adapter()
	if err != nil {
		fmt.Fprintf(os.Stderr, "model: %v\n", err)
		os.Exit(1)
	}
	// A retry is silence from the user's point of view, and silence in an
	// interactive session is indistinguishable from a hang. Say what happened.
	type notifier interface{ SetNotify(func(string)) }
	if n, ok := a.(notifier); ok {
		n.SetNotify(func(msg string) {
			fmt.Fprintf(os.Stderr, "  %s\n", msg)
		})
	}
	return a
}

// doctor verifies the endpoint actually works before the user debugs it
// through a failing agent run.
func (a *App) doctor(workspace string) int {
	cfg, err := a.loadConfig(workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		var ed *EditionError
		if errors.As(err, &ed) {
			return 2
		}
		return 1
	}
	provider, err := cfg.Provider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}

	fmt.Printf("%s %s\n\n", ui.NewStyle(os.Stdout).Cyan(ui.Glyph),
		ui.NewStyle(os.Stdout).Bold("abhed doctor"))
	fmt.Printf("workspace   %s\n", workspace)
	fmt.Printf("provider    %s (%s)\n", cfg.Model.Default, provider.Type)
	fmt.Printf("endpoint    %s\n", provider.BaseURL)
	fmt.Printf("model       %s\n", provider.Model)
	fmt.Printf("mode        %s\n", orDefault(cfg.Permissions.Mode, "default"))
	unknown := printUnknown(os.Stdout, cfg)
	if sb, err := buildSandbox(cfg, workspace); err == nil {
		label := string(sb.Tier())
		if sb.Tier() == sandbox.TierNone {
			label += "  ⚠"
		}
		fmt.Printf("sandbox     %s — %s\n", label, sb.Describe())
	} else {
		fmt.Printf("sandbox     UNAVAILABLE — %v\n", err)
	}
	if files := agent.DiscoverMemoryFiles(workspace); len(files) > 0 {
		fmt.Printf("memory      %s\n", strings.Join(files, ", "))
	}
	if mw, err := a.buildAuth(context.Background(), cfg, workspace); err != nil {
		fmt.Printf("auth        %s\n            UNAVAILABLE — %v\n", authLabel(cfg, nil), err)
	} else {
		fmt.Printf("auth        %s\n", authLabel(cfg, mw))
		// A provider that can prove itself does so here, so a misconfigured
		// issuer is a doctor finding rather than the first user's error page.
		for _, p := range mw.Providers {
			if c, ok := p.(auth.Checker); ok {
				if err := c.Check(context.Background()); err != nil {
					fmt.Printf("            %s UNAVAILABLE — %v\n", p.Name(), err)
				} else {
					fmt.Printf("            %s ready\n", p.Name())
				}
			}
		}
	}
	fmt.Printf("web search  %s\n", webSearchLabel(cfg))
	if reg, _ := buildSkills(cfg); reg.Len() > 0 {
		fmt.Printf("skills      %d loaded: %s\n", reg.Len(),
			strings.Join(reg.Names(), ", "))
	}
	if cfg.K8s.Enabled {
		writes := "read-only"
		if cfg.K8s.AllowWrites {
			writes = "reads + writes (every write needs approval)"
		}
		fmt.Printf("kubernetes  %s\n", writes)
		if c, err := k8s.Open(k8s.Config{Kubeconfig: cfg.K8s.Kubeconfig,
			Context: cfg.K8s.Context, Namespace: cfg.K8s.Namespace,
			Token: os.Getenv("ABHED_K8S_TOKEN")}); err != nil {
			fmt.Printf("            UNAVAILABLE — %v\n", err)
		} else {
			fmt.Printf("            context %s\n            namespace %s · server %s\n",
				c.Name, c.Namespace, c.Server)
		}
	}
	if cfg.SSH.Enabled && len(cfg.SSH.Hosts) > 0 {
		for _, h := range cfg.SSH.Hosts {
			warn := ""
			if h.InsecureSkipHostKeyCheck {
				warn = "  ⚠ host key verification disabled"
			}
			fmt.Printf("ssh host    %s → %s@%s%s\n", h.Name, h.User, h.Addr, warn)
		}
	}
	for _, c := range cfg.RAG.Corpora {
		if c.Enabled {
			fmt.Printf("rag corpus  %s → %s\n", c.Name, c.URL)
		}
	}
	fmt.Printf("storage     %s\n", storageLabel(cfg))
	if cfg.Storage.Driver == "postgres" {
		st, closeFn, err := openStore(context.Background(), cfg)
		if err != nil {
			fmt.Printf("            UNAVAILABLE — %v\n", err)
		} else {
			if pg, ok := st.(*store.Postgres); ok {
				printStoreStatus(pg)
			}
			closeFn()
		}
	}
	if cfg.Retrieval.Enabled {
		if ix, err := openIndex(context.Background(), cfg, workspace); err == nil {
			d, t, v, _ := ix.Stats()
			fmt.Printf("index       %d chunks · %d terms · %d vectors\n", d, t, v)
		} else {
			fmt.Printf("index       UNAVAILABLE — %v\n", err)
		}
	}
	if servers := cfg.MCP.Servers; len(servers) > 0 {
		gw := mcp.NewGateway()
		gw.Connect(context.Background(), mcpConfigs(cfg))
		status := gw.Status()
		gw.Close()
		if len(status) > 0 {
			fmt.Printf("mcp         %s\n", strings.Join(status, ", "))
		} else {
			fmt.Printf("mcp         %d configured, none connected\n", len(servers))
		}
	}
	fmt.Println()

	adapter := buildAdapter(provider)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Print("checking endpoint... ")
	stream, err := adapter.Complete(ctx, model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "Reply with the single word: ok"}},
		// Generous for a one-word answer, because a hybrid-reasoning model
		// spends this budget on its thinking phase FIRST. gemma4:26b returned
		// empty content and finish_reason=length at 32 tokens — a healthy
		// model reported as broken.
		MaxTokens: 512,
	})
	if err != nil {
		fmt.Printf("FAILED\n  %v\n", err)
		fmt.Println("\nCheck that the endpoint is reachable and the model name is correct.")
		return 1
	}
	var got strings.Builder
	for c := range stream {
		if c.Type == model.ChunkText {
			got.WriteString(c.Text)
		}
		if c.Type == model.ChunkError {
			fmt.Printf("FAILED\n  %v\n", c.Err)
			return 1
		}
	}
	answer := strings.TrimSpace(got.String())
	if answer == "" {
		// Distinguish "said nothing" from "said something unexpected": the
		// first usually means the token budget went to reasoning, which is a
		// configuration problem, not a broken endpoint.
		fmt.Printf("ok\n  response was empty — if this model reasons before " +
			"answering, raise context.max_tokens\n")
	} else {
		fmt.Printf("ok\n  response: %q\n", answer)
	}

	// Tool calling is the capability the agent actually depends on.
	fmt.Print("checking tool calling... ")
	stream, err = adapter.Complete(ctx, model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "List files matching *.go using the glob tool."}},
		Tools: []model.ToolDef{{
			Name:        "glob",
			Description: "Find files matching a glob pattern.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`),
		}},
		MaxTokens: 256,
	})
	if err != nil {
		fmt.Printf("FAILED\n  %v\n", err)
		return 1
	}
	calls := 0
	for c := range stream {
		if c.Type == model.ChunkToolCall {
			calls++
			fmt.Printf("ok\n  called %s with %s\n", c.ToolCall.Name, c.ToolCall.Args)
		}
	}
	if calls == 0 {
		fmt.Println("FAILED")
		fmt.Println("  The model did not emit a tool call. Abhed requires tool-calling support.")
		fmt.Println("  Check that the serving stack has a tool-call parser enabled for this model.")
		return 1
	}

	// The sandbox is checked by running something through it, not by asking
	// whether it is configured. Inside a hardened container bubblewrap could
	// not mount /proc and every command failed; doctor said "process — via
	// bwrap" and nothing else, because it never tried. Now it tries.
	fmt.Print("checking sandbox exec... ")
	if sb, err := buildSandbox(cfg, workspace); err != nil {
		fmt.Printf("SKIPPED\n  %v\n", err)
	} else {
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, err := sb.Command(sctx, workspace, "echo abhed-sandbox-ok").CombinedOutput()
		cancel()
		if err != nil || !strings.Contains(string(out), "abhed-sandbox-ok") {
			fmt.Println("FAILED")
			fmt.Printf("  tier %s could not run a command: %v\n", sb.Tier(), err)
			if msg := strings.TrimSpace(string(out)); msg != "" {
				fmt.Printf("  %s\n", msg)
			}
			fmt.Println("  The agent's bash tool would fail the same way. Fix the sandbox before relying on it.")
			return 1
		}
		fmt.Printf("ok\n  ran a command under the %s tier\n", sb.Tier())
	}

	return doctorVerdict(os.Stdout, unknown)
}

// doctorVerdict ends a doctor run whose checks all passed: ready, unless the
// configuration has keys nothing reads.
func doctorVerdict(w io.Writer, unknown bool) int {
	if unknown {
		fmt.Fprintln(w, "\nNot ready: the configuration has keys nothing reads (listed above). Correct or remove them.")
		return 1
	}
	fmt.Fprintln(w, "\nReady.")
	return 0
}

// printUnknown lists the configuration's unknown keys and reports whether there were any.
func printUnknown(w io.Writer, cfg config.Config) bool {
	for i, u := range cfg.Unknown {
		label := "            "
		if i == 0 {
			label = "config      "
		}
		fmt.Fprintf(w, "%s%s  ⚠\n", label, u)
	}
	return len(cfg.Unknown) > 0
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

// providersCmd lists the model providers this build supports.
//
// The set is whatever registered itself at init, so it is accurate for the
// binary in hand rather than for the documentation — which matters for a build
// that deliberately drops the cloud providers for an air-gapped install.
func providersCmd() int {
	fmt.Println("Model providers in this build:")
	fmt.Println()
	for _, d := range model.Describe() {
		fmt.Println("  " + d)
	}
	fmt.Println()
	fmt.Println("Set one as \"type\" in .abhed/config.json under model.providers.")
	fmt.Println("Sampling parameters go in that provider's \"params\" object;")
	fmt.Println("a parameter the provider cannot honour is reported at startup")
	fmt.Println("rather than silently ignored.")
	return 0
}

// toAgentTodos converts the tool's items to the event payload's.
//
// The two types are deliberately separate: internal/tools must not import the
// agent package, or every tool would drag the event schema behind it.
func toAgentTodos(items []tools.TodoItem) []agent.Todo {
	out := make([]agent.Todo, 0, len(items))
	for _, i := range items {
		out = append(out, agent.Todo{ID: i.ID, Text: i.Text, Status: i.Status})
	}
	return out
}

// forkPoints lists the steps a session can be forked at, so the user has
// something to name rather than guessing a sequence number.
func forkPoints(r *ui.Renderer, events []agent.Event) {
	s := r.Style()
	shown := 0
	for _, ev := range events {
		var label string
		switch ev.Type {
		case agent.EvUserMessage:
			var m agent.Message
			if json.Unmarshal(ev.Payload, &m) == nil {
				label = "you: " + firstLine(m.Text, 60)
			}
		case agent.EvActionRequested:
			var a agent.ActionRequested
			if json.Unmarshal(ev.Payload, &a) == nil {
				label = a.Tool + " " + firstLine(string(a.Args), 50)
			}
		default:
			continue
		}
		fmt.Printf("    %s  %s\n", s.Dim(fmt.Sprintf("%4d", ev.Seq)), label)
		shown++
		if shown >= 30 {
			fmt.Println(s.Dim("    …"))
			break
		}
	}
}

func firstLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// attachExtensionSummarizer lets an extension supply or refuse a compaction
// summary. Compaction is the one place the harness discards information on
// purpose, and the default summarizer cannot know what this deployment must
// keep.
func attachExtensionSummarizer(c *agent.Compactor, h *extension.Host, sessionID string) {
	if c == nil || h == nil || h.Len() == 0 {
		return
	}
	c.Summarizer = func(msgs []model.Message) (string, bool) {
		out := make([]extension.Message, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, extension.Message{
				Role: string(m.Role), Content: m.Content,
			})
		}
		return h.OnBeforeCompact(context.Background(), sessionID, out)
	}
}

// hawkeyeCmd reports on a finished session: from an exported events file, or
// by id from the durable store. It never needs a model or a network.
func hawkeyeCmd(workspace string, args []string) int {
	fl := flag.NewFlagSet("hawkeye", flag.ExitOnError)
	out := fl.String("o", "", "write the report here (.html or .json); the summary still prints")
	fl.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: abhed hawkeye [-o report.html] <events.json | session-id>")
	}
	_ = fl.Parse(args)
	if fl.NArg() != 1 {
		fl.Usage()
		return 2
	}
	target := fl.Arg(0)

	var events []agent.Event
	id := target
	if data, err := os.ReadFile(target); err == nil { //nolint:gosec // the operator names the file
		if err := json.Unmarshal(data, &events); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %s is not an exported events file: %v\n", target, err)
			return 1
		}
		if len(events) > 0 {
			id = events[0].SessionID
		}
	} else {
		cfg, err := config.Load(workspace)
		if err != nil {
			fail(err)
		}
		if cfg.Storage.Driver != "postgres" {
			fmt.Fprintf(os.Stderr, "abhed: %q is not a file, and there is no durable store to look it up in.\n"+
				"Export a session with /export session.json, or configure storage.driver.\n", target)
			return 1
		}
		st, closeStore, err := openStore(context.Background(), cfg)
		if err != nil {
			fail(err)
		}
		defer closeStore()
		if events, err = st.Events(target); err != nil {
			fail(err)
		}
	}
	if len(events) == 0 {
		fmt.Fprintf(os.Stderr, "abhed: no events for %s\n", target)
		return 1
	}

	rep := hawkeye.Analyze(id, events)
	fmt.Print(hawkeye.Text(rep))
	if *out != "" {
		if err := writeHawkeye(*out, rep); err != nil {
			fail(err)
		}
		fmt.Printf("\n  wrote %s\n", *out)
	}
	// A record with holes in it is an exit code a pipeline can act on.
	for _, f := range rep.Findings {
		if f.Severity == hawkeye.Critical {
			return 3
		}
	}
	return 0
}

func writeHawkeye(path string, rep hawkeye.Report) error {
	var data []byte
	var err error
	if strings.HasSuffix(path, ".json") {
		data, err = json.MarshalIndent(rep, "", "  ")
	} else {
		var page string
		page, err = hawkeye.HTML(rep)
		data = []byte(page)
	}
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// migrateCmd applies the schema as the owning role and grants the runtime role
// what the server needs. It is the one place the owner's credentials are used.
func migrateCmd(workspace string, extensions []store.Extension) int {
	cfg, err := config.Load(workspace)
	if err != nil {
		fail(err)
	}
	if cfg.Storage.Driver != "postgres" {
		fmt.Fprintln(os.Stderr, "abhed: storage.driver is not postgres; there is nothing to migrate")
		return 2
	}
	if cfg.Storage.MigrateDSN == "" {
		fmt.Fprintln(os.Stderr, "abhed: no owner connection. Set ABHED_MIGRATE_DATABASE_URL (or storage.migrate_dsn) to the role\n"+
			"that should own the tables. It must differ from the role in storage.dsn, which the server runs as.")
		return 2
	}
	runtime, err := pgx.ParseConfig(cfg.Storage.DSN)
	if err != nil {
		fail(fmt.Errorf("storage.dsn: %w", err))
	}
	if err := store.Provision(context.Background(), store.ProvisionConfig{
		OwnerDSN: cfg.Storage.MigrateDSN, RuntimeRole: runtime.User, Extensions: extensions,
	}); err != nil {
		fail(err)
	}
	fmt.Printf("Schema applied. %q may insert and read events and cannot change or remove them.\n"+
		"Keep the owner's credentials off the host that runs the server.\n", runtime.User)
	return 0
}

// printStoreStatus reports what the connection found, not what the config
// hoped for: how much is stored, and whether this role could alter it.
func printStoreStatus(pg *store.Postgres) {
	if sessions, events, err := pg.Stats(context.Background()); err == nil {
		fmt.Printf("            %d sessions · %d events persisted\n", sessions, events)
	}
	if pg.RecordProtected() {
		fmt.Println("            record protected — this role cannot change or remove events")
		return
	}
	fmt.Println("            record NOT protected from this server's credentials (storage.single_role)")
}

// openVault opens the secrets store. A missing file is an empty store, so a
// deployment with no secrets pays nothing and needs no configuration.
func openVault() *secrets.Store {
	path, err := secrets.DefaultPath()
	if err != nil {
		path = ".abhed-secrets-unavailable"
	}
	return secrets.Open(path)
}

// vaultNames lists what the model may ask for. An unreadable store lists
// nothing: the failure surfaces when a secret is used, with its reason.
func vaultNames(v *secrets.Store) []string {
	names, err := v.Names()
	if err != nil {
		return nil
	}
	return names
}

// secretCmd manages the store: set NAME (value on stdin or prompted), list, rm NAME.
func secretCmd(args []string) int {
	vault := openVault()
	fail := func(err error) int { fmt.Fprintf(os.Stderr, "abhed: %v\n", err); return 1 }
	usage := func() int {
		fmt.Fprintln(os.Stderr, "usage: abhed secret set NAME | list | rm NAME\n"+
			"  The value is read from stdin, or prompted without echo on a terminal.\n"+
			"  Stored in "+vault.Path()+" ("+secrets.EnvFile+" overrides), mode 600, never in config.\n"+
			"  A session may use a secret only under an allow rule: \"allow\": [\"secret(NAME)\"].")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "list":
		names, err := vault.Names()
		if err != nil {
			return fail(err)
		}
		if len(names) == 0 {
			fmt.Println("no secrets stored in " + vault.Path())
			return 0
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return 0
	case "rm", "remove":
		if len(args) != 2 {
			return usage()
		}
		if err := vault.Remove(args[1]); err != nil {
			return fail(err)
		}
		fmt.Printf("removed %s\n", args[1])
		return 0
	case "set":
		if len(args) != 2 {
			return usage()
		}
		var value string
		if term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Fprintf(os.Stderr, "value for %s (not echoed): ", args[1])
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return fail(err)
			}
			value = string(b)
		} else {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fail(err)
			}
			value = strings.TrimRight(string(b), "\r\n")
		}
		if err := vault.Set(args[1], value); err != nil {
			return fail(err)
		}
		fmt.Printf("stored %s in %s\n", args[1], vault.Path())
		return 0
	}
	return usage()
}
