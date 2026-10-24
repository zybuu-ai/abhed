package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Modes of model.ollama.assist.
const (
	// ModeAsk offers to start, install and pull, asking each time (a
	// standing "always" covers starting only).
	ModeAsk = "ask"
	// ModeLocal offers to start the local server, never to install or
	// download: for a machine with no network or a policy against downloads.
	ModeLocal = "local"
	// ModeOff offers nothing: the message and the manual steps only.
	ModeOff = "off"
)

// ParseMode reads a mode; "" is ask.
func ParseMode(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", ModeAsk:
		return ModeAsk, true
	case ModeLocal:
		return ModeLocal, true
	case ModeOff:
		return ModeOff, true
	}
	return "", false
}

// Ops.
const (
	OpStart   = "start"
	OpInstall = "install"
	OpPull    = "pull"
)

// Assist is one surface's way of acting for the person: what it may do and
// where each attempt is recorded.
type Assist struct {
	Env  Env
	Mode string
	// Surface is cli, tui, server or acp.
	Surface string
	// InAgent says why this process runs inside an agent's command; then
	// nothing is done, as the agent must not reach this through a nested
	// Abhed.
	InAgent string
	// Admin is whether the person may act on this host: always on their
	// own machine, an administrator on a server.
	Admin bool
	// Record writes an attempt to the session's record; nil records nothing.
	Record func(agent.EventType, agent.OllamaAction)
}

var evOf = map[string]agent.EventType{OpStart: agent.EvOllamaStart, OpInstall: agent.EvOllamaInstall, OpPull: agent.EvOllamaPull}

// May says why op may not be done here, or nil when it may (after asking).
func (a Assist) May(op string, t Target, st Status) *Problem {
	steps := a.Env.Steps(t, st)
	switch {
	case a.InAgent != "":
		return &Problem{Kind: KindInAgent, Message: "This runs inside an agent's command (" + a.InAgent + "), so Abhed will not start, install or download anything from here.", Steps: steps}
	case !t.Loopback:
		return &Problem{Kind: KindRemote, Message: t.API + " is on another machine; Abhed acts only on this one.", Steps: steps, Retry: true}
	case !a.Admin:
		return &Problem{Kind: KindNotAdmin, Message: "Only a signed-in administrator of this server can start, install or download Ollama on it.",
			Steps: append([]string{"On the server's host, in a terminal: abhed ollama check " + t.Provider + "   (it shows each step and asks first), or do the steps below there"}, steps...), Retry: true}
	case a.Mode == ModeOff:
		return &Problem{Kind: KindAssistOff, Message: "Abhed is set not to start, install or download Ollama here (model.ollama.assist is off).", Steps: steps, Retry: true}
	case a.Mode == ModeLocal && op != OpStart:
		return &Problem{Kind: KindAssistOff, Message: "Abhed is set not to install or download anything here (model.ollama.assist is local).", Steps: steps, Retry: true}
	case op == OpPull && st.Running && st.Version == "":
		// It answers the OpenAI API but not Ollama's own, so it may not be Ollama.
		return &Problem{Kind: KindNotOllama, Message: "The server at " + t.API + " answers the OpenAI API but not Ollama's own, so Abhed will not ask it to download anything.", Steps: steps, Retry: true}
	}
	return nil
}

// Note records an attempt that did not run: declined, or refused by May.
func (a Assist) Note(op, outcome string, t Target, p *Problem) {
	ev := a.action(op, outcome, t)
	if p != nil {
		ev.Problem, ev.Error = p.Kind, p.Message
	}
	a.record(op, ev)
}

func (a Assist) action(op, outcome string, t Target) agent.OllamaAction {
	return agent.OllamaAction{Op: op, Outcome: outcome, Provider: t.Provider, Model: t.Model, Base: t.API, By: agent.ByUser, Surface: a.Surface}
}

func (a Assist) record(op string, ev agent.OllamaAction) {
	if a.Record != nil {
		a.Record(evOf[op], ev)
	}
}

// outcomeOf is the recorded outcome of a problem.
func outcomeOf(p *Problem) string {
	switch {
	case p == nil:
		return agent.OllamaOK
	case p.Kind == KindCancelled:
		return agent.OllamaCancelled
	case p.Kind == KindManual:
		return agent.OllamaManual
	}
	return agent.OllamaFailed
}

// Start starts the server for t, after the person agreed (consent is
// asked or always), and records the attempt. Starts of one target are
// serialized; one that finds the server already answering starts nothing.
func (a Assist) Start(ctx context.Context, t Target, st Status, consent string) (Started, *Problem) {
	if p := a.May(OpStart, t, st); p != nil {
		a.Note(OpStart, agent.OllamaRefused, t, p)
		return Started{}, p
	}
	lock := startLock(t.API)
	lock.Lock()
	defer lock.Unlock()
	if v, err := a.Env.version(ctx, t); err == nil {
		return Started{Version: v}, nil // another request started it
	}
	ev := a.action(OpStart, "", t)
	ev.Consent = consent
	plan, p := a.Env.PlanStart(t, st)
	var s Started
	if p == nil {
		ev.Command = plan.Display
		s, p = a.Env.Start(ctx, t, st, plan)
		ev.PID = s.PID
	}
	ev.Outcome = outcomeOf(p)
	if p != nil {
		ev.Problem, ev.Error = p.Kind, p.Message
	}
	a.record(OpStart, ev)
	return s, p
}

// Install runs plan after the person agreed this time, and records it.
func (a Assist) Install(ctx context.Context, t Target, st Status, plan InstallPlan, stdin io.Reader, stdout, stderr io.Writer) *Problem {
	if p := a.May(OpInstall, t, st); p != nil {
		a.Note(OpInstall, agent.OllamaRefused, t, p)
		return p
	}
	ev := a.action(OpInstall, "", t)
	ev.Consent, ev.Command, ev.Source = "asked", plan.Display, plan.Source
	p := a.Env.Install(ctx, plan, stdin, stdout, stderr)
	ev.Outcome = outcomeOf(p)
	if p == nil && plan.Kind == InstallOpen {
		ev.Outcome = agent.OllamaManual
	}
	if p != nil {
		ev.Problem, ev.Error = p.Kind, p.Message
	}
	a.record(OpInstall, ev)
	return p
}

// Pull downloads t.Model after the person agreed this time, and records it.
func (a Assist) Pull(ctx context.Context, t Target, st Status, fit Fit, progress func(Progress)) *Problem {
	if p := a.May(OpPull, t, st); p != nil {
		a.Note(OpPull, agent.OllamaRefused, t, p)
		return p
	}
	ev := a.action(OpPull, "", t)
	ev.Consent, ev.Source, ev.Bytes = "asked", a.Env.Registry, fit.Bytes
	ev.Command = t.PullCommand()
	p := a.Env.Pull(ctx, t, st, progress)
	ev.Outcome = outcomeOf(p)
	if p != nil {
		ev.Problem, ev.Error = p.Kind, p.Message
	}
	a.record(OpPull, ev)
	return p
}

// consent is the person's standing choice, kept in StateDir, for one program.
type consent struct {
	Start string `json:"start,omitempty"` // always
	// Program is the resolved path the choice was made for; a different
	// program on PATH is asked about again.
	Program string    `json:"program,omitempty"`
	At      time.Time `json:"at,omitzero"`
}

func (e Env) consentFile() string { return filepath.Join(e.StateDir, "ollama.json") }

// StartAlways reports whether the person said to start program without asking.
func (e Env) StartAlways(program string) bool {
	if e.StateDir == "" || program == "" {
		return false
	}
	b, err := os.ReadFile(e.consentFile())
	if err != nil {
		return false
	}
	var c consent
	return json.Unmarshal(b, &c) == nil && c.Start == "always" && c.Program == program
}

// SetStartAlways keeps the choice to start program without asking, or with
// on false forgets any such choice.
func (e Env) SetStartAlways(on bool, program string) error {
	if e.StateDir == "" {
		return errors.New("there is no state folder to keep the choice in")
	}
	if !on {
		err := os.Remove(e.consentFile())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if program == "" {
		return errors.New("there is no program to keep the choice for")
	}
	if err := os.MkdirAll(e.StateDir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(consent{Start: "always", Program: program, At: time.Now().UTC()})
	tmp := e.consentFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, e.consentFile())
}
