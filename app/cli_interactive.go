package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/server"
)

func interactive(ctx context.Context, a *App, store server.EventStore, r *ui.Renderer,
	adapter model.Adapter, registry *tools.Registry, pol *policy.Engine,
	approver agent.Approver, sess *tools.Session, cfg agent.Config,
	appCfg config.Config, provider config.ProviderConfig, workspace string,
	budget *agent.Budget, extHost *extension.Host, start interactiveStart) int {

	s := r.Style()
	cfg.TurnsPerMessage = turnsPerMessage(appCfg, cfg.MaxTurns)
	sandboxLabel := start.sandbox.Label()
	if !appCfg.Sandbox.AllowNetwork {
		sandboxLabel += " · no network"
	}

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
	setupTerminal(editor, r, workspace)
	// Raw mode turns off the terminal's own newline translation, so every
	// print in the program would otherwise staircase down the screen.
	restoreStreams := editor.Capture()
	defer restoreStreams()
	// The banner is drawn once the terminal has said what its background
	// is, in the theme that suits it, and joins the transcript.
	fmt.Print(ui.Banner(s, a.version, provider.Model, workspace,
		sandboxLabel, storageLabel(appCfg)))
	fmt.Printf("\n%s\n\n", s.Dim("Type a task, or /help. Esc interrupts, Ctrl-C twice exits."))
	// The endpoint check started with the session. A server that is down is
	// named now; one still being dialled is left to the first task.
	if err := start.probe.firstResult(100 * time.Millisecond); err != nil {
		fmt.Printf("  %s %s\n", s.Red("!"), friendlyModelError(err, appCfg.Model.Default, provider))
	}

	// Approval input rides the one stdin reader the editor owns. Without this
	// the approver opened a second reader on stdin, racing the editor for each
	// keystroke and waiting for a "\n" raw mode never sends. Prepare also
	// pauses the thinking indicator so its animation does not overwrite the
	// prompt.
	prompter := ui.NewPrompter()
	defer prompter.Close()
	if ap, ok := approver.(*ui.Approver); ok {
		ap.Prepare = func(ctx context.Context) (func() (string, bool), func()) {
			wasThinking := r.PauseThinking()
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
		// On a terminal the question is the dock's guarded dialog.
		approver = dialogApprover(ap, editor, r, sess)
	}

	lines := make(chan string)
	interruptCh := make(chan struct{})
	readErr := make(chan struct{})
	go readInput(editor, lines, interruptCh, readErr)
	turn := 0
	// Outside the conversation: /clear starts a new loop, and a budget built
	// with it would reset the allowance. It is the subagents' budget too.
	turnBudget := budget
	// Session-level state the slash commands operate on.
	sessionState := &cliState{
		store: store, appCfg: appCfg, sess: sess,
		workspace: sess.Root, adapter: adapter, provider: provider, overlay: pol.Session,
		turnLimit: cfg.MaxTurns, hooks: extHost, pol: pol,
		sandbox: start.sandbox, set: start.set, registry: registry, version: a.version,
		checkpoint: r.Checkpoint,
	}
	if ap, ok := approver.(*ui.Approver); ok {
		sessionState.scopes = ap.Session
	}
	// Commands show and ask through the terminal; without one they ask on
	// the typed lines, which the steering loop reads during a run.
	if editor.Raw() {
		sessionState.surface = ui.NewSurface(editor, nil)
	} else {
		sessionState.surface = ui.NewLineSurface(ui.LazyStdout{}, s, lineAnswers{lines: lines, ended: readErr})
		sessionState.surfaceReadsLines = true
	}
	ft := startFooter(editor, r, sessionState, pol, workspace)
	// The work panel under the input lists subagents and background jobs;
	// in the line mode only their completion notices and /tasks remain.
	panel := newWorkPanel(r, store, editor)
	sessionState.panel = panel
	editor.SetWork(panel.rows, panel.view, panel.send)
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go panel.watch(stopWatch)
	sessionState.fresh()
	// Wake runs the background manager asks for, run by the loop below.
	wakeCh := make(chan []string, 1)
	// One conversation per session: every task continues the same loop and
	// record until /clear, and /fork and /resume change what it continues from.
	sessionState.recordStart = start.recordStart
	sessionState.open = func(id string, after int64) *agent.Loop {
		rec := agent.NewRecorder(store, id, "")
		// Read again as the store changes: bash reads it at each call.
		rec.Redact = openVault().Session()
		// What is held for the record is written after the steps already there.
		rec.Advance(after)
		start.onOpen(rec)
		// A new conversation records its start now; a continued one when it is
		// claimed, and a rebuild of the one already started never again.
		switch {
		case after == 0:
			sessionState.startOwed = 0
			if start.recordStart != nil {
				start.recordStart(rec, 0)
			}
			sessionState.startedID = id
		case id != sessionState.startedID:
			sessionState.startOwed = after
		}
		// Built on the startup adapter, whose name the prompt carries, then moved
		// to the one selected now, so a /model switch holds and the prompt follows it.
		loop := agent.NewLoop(adapter, registry, pol, approver, sess, rec, cfg)
		loop.Compactor = agent.NewCompactor(adapter, cfg.CompactAt)
		loop.EnablePlanExit()
		loop.SetAdapter(sessionState.adapter)
		loop.Provider = sessionState.appCfg.Model.Default
		loop.Budget = turnBudget
		loop.Suggest = cliSuggester(sessionState.appCfg, editor)
		toolset.Summarize(loop.Compactor, extHost, id)
		// Background tasks belong to the conversation and outlive a task;
		// their results are shown as they arrive, at the prompt too.
		loop.Work = agent.NewWork()
		agent.NewBackground(loop, toolset.BackgroundPolicy(sessionState.appCfg, agent.WakeAuto))
		loop.Background.SetHooks(agent.BackgroundHooks{
			// A wake waits while something is typed: that message will carry the result.
			CanWake: func() (bool, string) {
				if editor.Typing() {
					return false, "typing"
				}
				return true, ""
			},
			Wake: func(ids []string) bool {
				select {
				case wakeCh <- ids:
					return true
				default:
					return false
				}
			},
		})
		sessionState.endBackground()
		sessionState.loop, sessionState.sessionID = loop, id
		panel.attach(loop, id)
		sessionState.follow(store, id, r)
		sessionState.attachHooks(loop)
		sessionState.flushPending()
		return loop
	}
	defer sessionState.endBackground()
	// -c, -r and --fork-session choose the conversation before the first prompt.
	startSession(ctx, sessionState, r)

	eof := false
	// lastCtrlC is when Ctrl-C was pressed at the prompt with background
	// tasks running; a second within two seconds cancels them.
	var lastCtrlC time.Time
	idleTick := time.NewTicker(250 * time.Millisecond)
	defer idleTick.Stop()

	// runTurn drives one run: the prompt's own, or a wake. It returns an exit
	// code and true when the session should end.
	// woken marks the turn runTurn is running as a wake the session started
	// itself, whose "nothing to wake for" is not the person's error.
	woken := false
	runTurn := func(start func(ctx context.Context, loop *agent.Loop) (agent.TerminalReason, error)) (int, bool) {
		loop := sessionState.loop
		// A suggestion still being made is for a prompt no longer coming; it
		// stops, and what it recorded is drawn before the turn clears it.
		loop.StopSuggestion()
		sessionState.waitRendered(loop.Recorder.LastAppended())
		// Each task gets its own cancellable context so Ctrl-C interrupts the
		// task without killing the session.
		taskCtx, cancelCause := context.WithCancelCause(ctx)
		cancelTask := func() { cancelCause(nil) }
		before := loop.Usage()
		sessionState.undo.BeginTurn()

		// Run on a goroutine so the reader stays live: anything typed now is a
		// steering message, applied at the next turn boundary rather than
		// killing the run.
		finished := make(chan turnOutcome, 1)
		// The indicator runs from the moment the turn starts until the first
		// output arrives. A cold local model can take thirty seconds to its
		// first token, and an unmoving prompt in that window is
		// indistinguishable from a hang.
		// The turn owns the screen: the reader stays live for steering, but
		// stops painting a prompt over the output.
		editor.Quiet(true)
		ft.turn(true)
		panel.turn(true)
		ft.refresh(sessionState, pol)
		r.StartThinking()
		go func() {
			defer ui.RestoreOnPanic() // a panic in the turn must not leave the terminal raw
			reason, err := start(taskCtx, loop)
			finished <- turnOutcome{reason, err}
		}()

		var runErr error
		var runReason agent.TerminalReason
		var queued []string
		interrupts := 0
	steering:
		for {
			select {
			case <-editor.Stops():
				// Esc stops the turn and keeps the session: unlike Ctrl-C it
				// never counts toward exiting. Background shells and tasks go on.
				cancelCause(agent.Interrupt{Detail: agent.InterruptKept})
			case <-interruptCh:
				// Ctrl-C stops the turn, and a pending approval with it: its
				// wait ends on the cancelled context, so the call is refused.
				// Stop means stop: the background tasks go too.
				interrupts++
				if interrupts > 1 {
					r.StopThinking()
					fmt.Printf("  %s\n", s.Dim("interrupted again — exiting"))
				}
				go loop.Background.CancelAll(agent.TermUserInterrupt)
				stop := func() { cancelCause(agent.Interrupt{Detail: agent.InterruptStopped}) }
				if code, stopped := interruptTurn(interrupts, stop, finished, exitGrace); code != 0 {
					endOnExit(sessionState, stopped)
					return code, true
				}
				wasOn := r.PauseThinking()
				fmt.Printf("  %s\n", s.Dim("interrupting…"))
				if wasOn {
					r.StartThinking()
				}
			case o := <-finished:
				// A steer that arrived after the run last looked is not left
				// waiting: a run that completed, or a wake stopped at its cap,
				// goes on for it, as the server's does.
				if runsOnFor(taskCtx, o, len(loop.Queued())) {
					go func() {
						reason, err := loop.RunQueued(taskCtx)
						finished <- turnOutcome{reason, err}
					}()
					continue
				}
				// The turn is over however it ended; the indicator goes with it.
				r.StopThinking()
				runErr, runReason = o.err, o.reason
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
				// A decision key typed while an approval is waiting is the
				// answer to it; any other line steers, and says the approval
				// still waits, so a line meant for the agent is never taken
				// as an answer by where it falls.
				if ui.Decision(msg) && prompter.Deliver(msg) {
					continue
				}
				noteStillWaiting(prompter, msg, "it steers the run")
				if msg == "" {
					continue
				}
				if msg == modeCycleLine {
					ft.cycleMode(sessionState, pol) // takes effect when the turn ends
					continue
				}
				if isCommandLine(msg) {
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
				if !editor.Raw() { // on a terminal the dock shows it, queued
					fmt.Printf("  %s\n", s.Dim("steering — applied at the next step"))
				}
			}
		}
		cancelTask()
		r.StopThinking() // every exit path converges here
		// What the run recorded is drawn before its usage is printed.
		sessionState.waitRendered(loop.Recorder.LastAppended())
		editor.Quiet(false)
		ft.turn(false)
		panel.turn(false)
		ft.applyPending(sessionState, pol)
		// The loop's usage covers the whole conversation; this task is the difference.
		spent := usageSince(before, loop.Usage())
		sessionState.accumulate(spent)

		if line := runErrorLine(runErr, woken); line != "" {
			fmt.Printf("%s %s\n", s.Red("error:"), friendlyModelError(runErr, sessionState.appCfg.Model.Default, sessionState.provider))
		}
		woken = false
		if runErr == nil {
			releaseRefused(sessionState, runReason)
		}
		settleTurn(sessionState, runErr)
		if !editor.Raw() { // on a terminal the footer carries both
			printUsage(r, spent)
			if line := sessionState.statusLine(ctx, string(pol.Mode)); line != "" {
				fmt.Printf("%s\n", s.Dim(line))
			}
		}
		if runReason == agent.TermMaxTurns {
			fmt.Println(s.Dim("  " + turnLimitNote(appCfg, cfg.MaxTurns)))
		}
		fmt.Println()

		// Anything typed as a command while the agent worked runs now, in the
		// order it was typed.
		for _, cmd := range queued {
			fmt.Printf("%s%s\n", ui.Prompt(s), cmd)
			if quit := dispatchLine(ctx, cmd, r, pol, sess, sessionState); quit {
				return 0, true
			}
			ft.refresh(sessionState, pol)
		}
		if ctx.Err() != nil {
			return 130, true
		}
		return 0, false
	}

	// A task given on the command line is the first line, as if typed.
	firstCh := make(chan string, 1)
	if start.first != "" {
		firstCh <- start.first
	}
	prompted := false
	for {
		// A turn a command asked for runs before the next line (input track).
		if t := sessionState.takeTurn(); t != nil {
			if sessionState.loop == nil {
				t.done() // dropped: what the command changed is put back
				continue
			}
			prompted = false
			code, quit := runTurn(func(ctx context.Context, loop *agent.Loop) (agent.TerminalReason, error) {
				return loop.RunMessage(ctx, t.msg)
			})
			t.done()
			if quit {
				return code
			}
			continue
		}
		// End of piped input waits for the background work, and for the
		// wakes its results start, before the session ends.
		if eof && sessionState.backgroundIdle() {
			fmt.Println()
			return 0
		}
		if !editor.Raw() && !eof && !prompted {
			fmt.Print(ui.Prompt(s))
			prompted = true
		}
		var line string
		select {
		case <-readErr:
			readErr, eof = nil, true
			prompter.Close()
			continue
		case <-ctx.Done():
			return 0
		case <-idleTick.C:
			continue
		case <-interruptCh:
			// At the prompt, Ctrl-C only abandons the line being typed, unless
			// background tasks run: then a second one within two seconds
			// cancels them.
			if n := sessionState.liveTasks(); n > 0 {
				if time.Since(lastCtrlC) < 2*time.Second {
					sessionState.loop.Background.CancelAll(agent.TermUserInterrupt)
					fmt.Printf("  %s\n", s.Dim(fmt.Sprintf("cancelled %d background task(s)", n)))
					lastCtrlC = time.Time{}
				} else {
					fmt.Printf("  %s\n", s.Dim(fmt.Sprintf("%d background task(s) running; Ctrl-C again within 2 s to cancel them", n)))
					lastCtrlC = time.Now()
				}
			}
			continue
		case ids := <-wakeCh:
			// A wake run, for background results, through the same driver as a task.
			if sessionState.loop == nil {
				continue
			}
			prompted = false
			woken = true
			if code, quit := runTurn(func(ctx context.Context, loop *agent.Loop) (agent.TerminalReason, error) {
				return loop.RunWoken(ctx, agent.Wake{By: "policy", TaskIDs: ids})
			}); quit {
				return code
			}
			continue
		case line = <-firstCh:
			fmt.Printf("%s%s\n", ui.Prompt(s), line)
		case line = <-lines:
			prompted = false
		}
		// An approval a background task is waiting on takes a line that is
		// exactly a decision key, when input arrives as lines. Any other line
		// is a prompt, with a note that the approval still waits.
		if ui.Decision(line) && prompter.Deliver(line) {
			continue
		}
		noteStillWaiting(prompter, line, "it was sent as a prompt")
		if line == "" {
			continue
		}
		// "exit" and "quit" without a slash are commands too. They were sent to
		// the model as prompts, which replied "Goodbye!" while the session
		// stayed open — the CLI ignoring the one word everyone tries first.
		if bare := strings.ToLower(strings.TrimSpace(line)); bare == "exit" || bare == "quit" {
			line = "/" + bare
		}
		if line == modeCycleLine {
			ft.cycleMode(sessionState, pol)
			continue
		}
		if isCommandLine(line) {
			if quit := dispatchLine(ctx, line, r, pol, sess, sessionState); quit {
				return 0
			}
			ft.refresh(sessionState, pol)
			continue
		}

		turn++
		if err := ensureConversation(ctx, sessionState); err != nil {
			fmt.Printf("  %s not continued: %v\n", s.Red("✕"), err)
			continue
		}
		// @ mentions are attached through the session's policy (input track).
		task, ok := expandForTurn(ctx, sessionState, r, line)
		if !ok {
			continue
		}
		// The first message carries its @ mentions (input). A plan proposed in
		// a turn is decided at its end; an approved one goes on as the next
		// message (governance).
		run := func(ctx context.Context, loop *agent.Loop) (agent.TerminalReason, error) {
			return loop.RunMessage(ctx, task)
		}
		for {
			if code, quit := runTurn(run); quit {
				return code
			}
			next := decidePlan(ctx, sessionState, pol, sessionState.surface)
			if next == "" {
				break
			}
			run = func(ctx context.Context, loop *agent.Loop) (agent.TerminalReason, error) {
				return loop.Run(ctx, next)
			}
		}
	}
}

// interactiveStart is what the command line gives an interactive session:
// a first task, and what session.started records.
type interactiveStart struct {
	first   string
	sandbox *lazySandbox
	probe   *endpointProbe
	set     *toolset.Set
	// onOpen runs for each conversation's recorder: it binds where a model
	// fallback is recorded. recordStart records session.started.
	onOpen      func(rec *agent.Recorder)
	recordStart func(rec *agent.Recorder, resumedAfter int64)
}

// turnOutcome is how a turn's run ended.
type turnOutcome struct {
	reason agent.TerminalReason
	err    error
}

// exitGrace is how long a second Ctrl-C waits for the turn to stop before exiting.
const exitGrace = 1500 * time.Millisecond

// interruptTurn handles the nth Ctrl-C of a turn: it cancels the turn, and on
// a second one waits up to grace for it to stop and returns 130 to exit, with
// whether the turn stopped in that time.
func interruptTurn(n int, cancel func(), finished <-chan turnOutcome, grace time.Duration) (int, bool) {
	cancel()
	if n < 2 {
		return 0, false
	}
	select {
	case <-finished:
		return 130, true
	case <-time.After(grace):
		return 130, false
	}
}

// endOnExit ends the session as a second Ctrl-C exits: a turn that stopped in
// time recorded its own end, and one still stopping is ended as interrupted.
func endOnExit(st *cliState, stopped bool) {
	if !stopped {
		endIfOpen(st, agent.TermUserInterrupt)
	}
}

// lineSource is the part of the line reader the input goroutine uses.
type lineSource interface {
	ReadLine() (string, error)
}

// readInput feeds typed lines to the session and reports each Ctrl-C: in raw
// mode Ctrl-C is a key, not a signal, so a turn only hears it through here.
func readInput(in lineSource, lines chan<- string, interrupts chan<- struct{}, readErr chan<- struct{}) {
	for {
		line, err := in.ReadLine()
		if ui.ErrInterrupted(err) {
			interrupts <- struct{}{}
			continue
		}
		if err != nil {
			close(readErr)
			return
		}
		lines <- strings.TrimSpace(line)
	}
}

// noteStillWaiting says, for a line that did not answer a waiting approval,
// that the approval still waits and what became of the line.
func noteStillWaiting(p *ui.Prompter, line, became string) {
	if !p.Waiting() || strings.TrimSpace(line) == "" {
		return
	}
	fmt.Printf("  an approval is still waiting: answer with its number, 1, 2 or 3; %s\n", became)
}

// runErrorLine is what a turn's error prints, "" for none. A wake the
// session started for results a prompted run has since taken finds nothing
// to do; that is not an error to show.
func runErrorLine(err error, woken bool) string {
	if err == nil || woken && errors.Is(err, agent.ErrNothingToWake) {
		return ""
	}
	return err.Error()
}

// runsOnFor reports whether a turn that just ended has a person's message
// waiting that it should run on for: it ended cleanly, completed or at a
// wake's cap, and something is queued.
func runsOnFor(ctx context.Context, o turnOutcome, queued int) bool {
	return o.err == nil && ctx.Err() == nil &&
		(o.reason == agent.TermCompleted || o.reason == agent.TermWakeLimit) && queued > 0
}
