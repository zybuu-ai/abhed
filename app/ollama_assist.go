package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/defaults"
	"github.com/zybuu-ai/abhed/internal/ollama"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
	"golang.org/x/term"
)

// Getting a chosen Ollama model working, in the CLI.
//
// When the person picks a model a local Ollama serves (at the start, or
// with /model), Abhed checks that Ollama is installed, answering and has the
// model. When it is not, it says what is wrong with the steps to fix it by
// hand, and offers the next step: starting the server needs a yes once (or
// a standing "always", which `abhed ollama forget` and /ollama forget take
// back); installing and pulling ask every time, showing the exact command,
// where it comes from and how big it is. Nothing is downloaded otherwise.
// Only the person's own choice reaches this: no tool calls it.

func init() {
	registerSlash(slashCmd{Name: "/ollama", Args: "[forget]", Help: "check, and get working, the current model's local Ollama; forget takes back \"always start\"", Group: "model", Order: 101, Run: slashOllama})
}

// ollamaRounds bounds the steps one check offers: install, start, pull, and
// a retry of each.
const ollamaRounds = 8

// ollamaFlow is one surface's conversation about a target.
type ollamaFlow struct {
	ui ui.Surface
	a  ollama.Assist
	// terminal is set for `abhed ollama` at a terminal, where an install's
	// own output is shown as it runs.
	terminal bool
	// out takes an install's own output when terminal is set.
	out io.Writer
	// tell is set when no answer can be read without taking typed or piped
	// input meant for the session: the problem and the steps are shown, and
	// nothing is offered.
	tell bool
	// lookup is set when the person asked for the check themselves (/ollama,
	// abhed ollama check): an install's or a pull's size is fetched from its
	// host at once. Otherwise (the session's start, a model switch) the
	// person is first asked whether to look it up, so an automatic check
	// makes no request off this machine.
	lookup bool
}

// mayLookUp asks, when the check was automatic, whether to ask host for a
// size; an explicit check needs no question.
func (f ollamaFlow) mayLookUp(ctx context.Context, st ollama.Status, what, host string) bool {
	if f.lookup {
		return true
	}
	id, err := f.ui.Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm, Title: what,
		Body:    []ui.Block{{Kind: ui.BlockNotice, Text: st.Problem.Message}},
		Ask:     "Look up its size at " + host + " and show what it would run? Nothing is downloaded yet.",
		Choices: []ui.Choice{{ID: "look", Label: "Look it up"}, {ID: ui.ChoiceNo, Label: "Not now"}}})
	return err == nil && id == "look"
}

// ensure checks t and offers each next step until the model is ready, the
// person says no, or a step fails and they do not retry. It reports
// whether the model is ready.
func (f ollamaFlow) ensure(ctx context.Context, t ollama.Target) bool {
	for range ollamaRounds {
		st := f.a.Env.Check(ctx, t)
		if st.Ready {
			return true
		}
		if f.tell {
			f.ui.Append(ui.Block{Kind: ui.BlockError, Text: st.Problem.Text() + "\n`abhed ollama check " + t.Provider + "` in a terminal offers each step."})
			return false
		}
		var again bool
		switch st.Next {
		case "start":
			again = f.start(ctx, t, st)
		case "install":
			again = f.install(ctx, t, st)
		case "pull":
			again = f.pull(ctx, t, st)
		default:
			f.ui.Append(ui.Block{Kind: ui.BlockError, Text: st.Problem.Text()})
			again = f.retry(ctx, "Check again?")
		}
		if !again {
			return false
		}
	}
	return f.a.Env.Check(ctx, t).Ready
}

// refused records and shows why a step is not offered, with the steps, and
// asks to retry.
func (f ollamaFlow) refused(ctx context.Context, op string, t ollama.Target, st ollama.Status, p *ollama.Problem) bool {
	f.a.Note(op, agent.OllamaRefused, t, p)
	msg := st.Problem.Message + "\n" + p.Text()
	f.ui.Append(ui.Block{Kind: ui.BlockError, Text: msg})
	if !p.Retry {
		return false
	}
	return f.retry(ctx, "Check again once that is done?")
}

func (f ollamaFlow) start(ctx context.Context, t ollama.Target, st ollama.Status) bool {
	if p := f.a.May(ollama.OpStart, t, st); p != nil {
		return f.refused(ctx, ollama.OpStart, t, st, p)
	}
	plan, p := f.a.Env.PlanStart(t, st)
	if p != nil {
		f.ui.Append(ui.Block{Kind: ui.BlockError, Text: p.Text()})
		return p.Retry && f.retry(ctx, "Check again?")
	}
	consent := "always"
	if !f.a.Env.StartAlways(plan.Program) {
		body := []ui.Block{{Kind: ui.BlockNotice, Text: st.Problem.Message},
			{Kind: ui.BlockNotice, Text: "Command: " + plan.Display}}
		if plan.Log != "" {
			body = append(body, ui.Block{Kind: ui.BlockNotice, Text: "Its output goes to " + plan.Log + "."})
		}
		if plan.AppBinds {
			body = append(body, ui.Block{Kind: ui.BlockNotice, Text: "This opens the Ollama app, which listens where its own settings say; Abhed does not control that."})
		} else {
			body = append(body, ui.Block{Kind: ui.BlockNotice, Text: "It listens on this machine only."})
		}
		body = append(body, ui.Block{Kind: ui.BlockNotice, Text: "It runs as you and keeps running when Abhed exits; stop it as you would any Ollama."})
		id, err := f.ui.Dialog(ctx, ui.DialogSpec{Kind: ui.DialogChoice, Title: "Start Ollama?", Body: body,
			Ask: "Start the Ollama server now?",
			Choices: []ui.Choice{{ID: "start", Label: "Start it"},
				{ID: "always", Label: "Start it, and always start it without asking (abhed ollama forget takes this back)", Widening: true},
				{ID: ui.ChoiceNo, Label: "No"}},
			Default: ui.ChoiceNo, Cancel: ui.ChoiceNo})
		switch {
		case err != nil || id == ui.ChoiceNo || id == "":
			f.a.Note(ollama.OpStart, agent.OllamaDeclined, t, nil)
			f.manual(st)
			return false
		case id == "always":
			if err := f.a.Env.SetStartAlways(true, plan.Program); err != nil {
				f.ui.Append(ui.Block{Kind: ui.BlockError, Text: "The choice to always start could not be kept: " + err.Error()})
			}
		default:
			consent = "asked"
		}
	} else {
		f.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "Starting Ollama (" + plan.Display + "): you chose to always start it; /ollama forget takes that back."})
	}
	f.ui.Notify(ui.Toast{Text: "starting Ollama…"})
	s, p := f.a.Start(ctx, t, st, consent)
	if p != nil {
		f.ui.Append(ui.Block{Kind: ui.BlockError, Text: p.Text()})
		return p.Retry && f.retry(ctx, "Try again?")
	}
	f.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: fmt.Sprintf("Ollama %s is running at %s. Abhed does not stop it when you exit.", s.Version, t.API)})
	return true
}

func (f ollamaFlow) install(ctx context.Context, t ollama.Target, st ollama.Status) bool {
	if p := f.a.May(ollama.OpInstall, t, st); p != nil {
		return f.refused(ctx, ollama.OpInstall, t, st, p)
	}
	plan := f.a.Env.PlanInstall()
	if plan.Kind == ollama.InstallManual {
		why := "Abhed does not install Ollama on this system itself; run the step below yourself."
		if plan.Display == ollama.ScriptCommand {
			why = "Abhed never runs the official install script, which runs sudo itself; run it yourself (source: " + plan.Source + ")."
		}
		f.ui.Append(ui.Block{Kind: ui.BlockError, Text: st.Problem.Message + " " + why + "\n" + (&ollama.Problem{Steps: f.a.Env.Steps(t, st)}).Text()})
		return f.retry(ctx, "Check again once it is installed?")
	}
	if plan.SizeURL != "" && plan.SizeNote == "" {
		if !f.mayLookUp(ctx, st, "Install Ollama?", hostOf(plan.SizeURL)) {
			f.manual(st)
			return false
		}
		f.a.Env.InstallSize(ctx, &plan)
	}
	body := []ui.Block{{Kind: ui.BlockNotice, Text: st.Problem.Message},
		{Kind: ui.BlockNotice, Text: "Command: " + plan.Display},
		{Kind: ui.BlockNotice, Text: "Source: " + plan.Source},
		{Kind: ui.BlockNotice, Text: "Size: " + orDefault(plan.SizeNote, "not known until the download starts")},
		{Kind: ui.BlockNotice, Text: "It runs as you, without sudo."}}
	if plan.Kind == ollama.InstallOpen {
		body[len(body)-1] = ui.Block{Kind: ui.BlockNotice, Text: "This opens the download page in your browser; you install the app from there, then come back."}
	}
	id, err := f.ui.Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm, Title: "Install Ollama?", Body: body, Ask: "Run this now?"})
	if err != nil || id != ui.ChoiceYes {
		f.a.Note(ollama.OpInstall, agent.OllamaDeclined, t, nil)
		f.manual(st)
		return false
	}
	var buf tailBuffer
	out := io.Writer(&buf)
	if f.terminal && f.out != nil {
		out = f.out
	}
	f.ui.Notify(ui.Toast{Text: "running " + plan.Display + "…"})
	if p := f.a.Install(ctx, t, st, plan, nil, out, out); p != nil {
		text := p.Text()
		if tail := buf.String(); tail != "" {
			text += "\nThe last of its output:\n" + tail
		}
		f.ui.Append(ui.Block{Kind: ui.BlockError, Text: text})
		return p.Retry && f.retry(ctx, "Try again?")
	}
	if plan.Kind == ollama.InstallOpen {
		f.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "The download page is open. Install Ollama, open it once, then check again."})
		return f.retry(ctx, "Check again once it is installed?")
	}
	f.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "Ollama is installed."})
	return true
}

func (f ollamaFlow) pull(ctx context.Context, t ollama.Target, st ollama.Status) bool {
	if p := f.a.May(ollama.OpPull, t, st); p != nil {
		return f.refused(ctx, ollama.OpPull, t, st, p)
	}
	if !f.mayLookUp(ctx, st, "Download "+t.Model+"?", hostOf(f.a.Env.Registry)) {
		f.manual(st)
		return false
	}
	fit, p := f.a.Env.CheckFit(ctx, t, st)
	if p != nil {
		f.a.Note(ollama.OpPull, agent.OllamaFailed, t, p)
		f.ui.Append(ui.Block{Kind: ui.BlockError, Text: p.Text()})
		return p.Retry && f.retry(ctx, "Check again?")
	}
	size := "not known (the registry did not say)"
	if fit.Bytes > 0 {
		size = ollama.HumanBytes(fit.Bytes)
	}
	body := []ui.Block{{Kind: ui.BlockNotice, Text: st.Problem.Message},
		{Kind: ui.BlockNotice, Text: "Command: " + t.PullCommand()},
		{Kind: ui.BlockNotice, Text: "Source: " + f.a.Env.Registry},
		{Kind: ui.BlockNotice, Text: "Size: " + size}}
	if fit.DiskFree > 0 {
		body = append(body, ui.Block{Kind: ui.BlockNotice, Text: "Free where Ollama keeps models: " + ollama.HumanBytes(int64(fit.DiskFree))})
	}
	for _, w := range fit.Warnings {
		body = append(body, ui.Block{Kind: ui.BlockError, Text: w})
	}
	if fit.Smaller != "" {
		body = append(body, ui.Block{Kind: ui.BlockNotice, Text: "To use " + fit.Smaller + " instead, choose No and set it as this provider's model."})
	}
	id, err := f.ui.Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm, Title: "Download " + t.Model + "?", Body: body,
		Ask: "Download it now? (Esc or Ctrl-C stops it; Ollama keeps what it fetched.)"})
	if err != nil || id != ui.ChoiceYes {
		f.a.Note(ollama.OpPull, agent.OllamaDeclined, t, nil)
		f.manual(st)
		return false
	}
	var last time.Time
	p = f.a.Pull(ctx, t, st, fit, func(pr ollama.Progress) {
		if time.Since(last) < time.Second && pr.Status != "success" {
			return
		}
		last = time.Now()
		text := pr.Status
		if pr.Total > 0 {
			text = fmt.Sprintf("%s %d%% of %s", pr.Status, pr.Completed*100/pr.Total, ollama.HumanBytes(pr.Total))
		}
		f.ui.Notify(ui.Toast{Text: t.PullCommand() + ": " + text})
	})
	if p != nil {
		f.ui.Append(ui.Block{Kind: ui.BlockError, Text: p.Text()})
		return p.Retry && ctx.Err() == nil && f.retry(ctx, "Try again?")
	}
	f.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: t.Model + " is downloaded."})
	return true
}

// manual shows the steps the person can run themselves after saying no.
func (f ollamaFlow) manual(st ollama.Status) {
	if st.Problem != nil {
		f.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "Nothing was run. " + st.Problem.Text()})
	}
}

// retry asks whether to check again; no answer is no.
func (f ollamaFlow) retry(ctx context.Context, ask string) bool {
	if ctx.Err() != nil {
		return false
	}
	id, err := f.ui.Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm, Title: "Ollama", Ask: ask,
		Choices: []ui.Choice{{ID: "retry", Label: "Check again"}, {ID: ui.ChoiceNo, Label: "Not now"}}})
	return err == nil && id == "retry"
}

// tailBuffer keeps the last of an install's output.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(bytes.ToValidUTF8(t.b, nil)))
}

// ollamaEnv is the machine the assist acts on; tests replace it.
var ollamaEnv = ollama.DefaultEnv

// cliOllama is the CLI's assist for its own state: the person's machine,
// recorded in the conversation (or held until one opens).
func (st *cliState) cliOllama(surface string) ollama.Assist {
	return toolset.OllamaAssist(st.appCfg, ollamaEnv(), surface, true, inAgentCommand(), func(t agent.EventType, ev agent.OllamaAction) {
		st.recordCLI(t, ev)
	})
}

func (st *cliState) surfaceName() string {
	if st.surfaceReadsLines {
		return "cli"
	}
	return "tui"
}

// ensureOllamaFor runs the flow for a configured provider name, when a
// local Ollama serves it, and reports whether it is ready (true when it is
// not an Ollama model at all).
// explicit is set for /ollama, which the person ran to check; the start of
// a session and a model switch are automatic.
func (st *cliState) ensureOllamaFor(ctx context.Context, sf ui.Surface, name string, explicit bool) bool {
	t, ok := toolset.OllamaTarget(st.appCfg, name)
	if !ok {
		return true
	}
	tell := st.surfaceReadsLines && !term.IsTerminal(int(os.Stdin.Fd()))
	return ollamaFlow{ui: sf, a: st.cliOllama(st.surfaceName()), tell: tell, lookup: explicit}.ensure(ctx, t)
}

// slashOllama is /ollama: check the current model's Ollama and offer what
// it needs, or /ollama forget to take back "always start".
func slashOllama(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	if len(args) > 0 {
		if args[0] != "forget" {
			return false, fmt.Errorf("/ollama takes forget, or nothing")
		}
		if err := ollamaEnv().SetStartAlways(false, ""); err != nil {
			return false, err
		}
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "Abhed will ask again before it starts Ollama."})
		return false, nil
	}
	name := e.st.appCfg.Model.Default
	if _, ok := toolset.OllamaTarget(e.st.appCfg, name); !ok {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: name + " is not served by a local Ollama."})
		return false, nil
	}
	if e.st.ensureOllamaFor(ctx, e.ui, name, true) {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: name + " is ready."})
	}
	return false, nil
}

// ollamaSteps is the message and manual steps for an Ollama model that
// failed, or "" when the provider is not one or is ready. It only reads.
func ollamaSteps(ctx context.Context, name string, p config.ProviderConfig) string {
	t, ok := ollama.Resolve(name, p.Type, p.BaseURL, p.Model, defaults.OllamaURL)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	st := ollamaEnv().Check(ctx, t)
	if st.Problem == nil {
		return ""
	}
	return st.Problem.Text()
}
