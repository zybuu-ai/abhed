package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/mode", Args: "<name>", Help: "default | accept-edits | plan | auto", Group: "mode", Order: 10, Run: slashMode})
}

// switchMode checks a mode against the managed configuration, as -mode is
// checked, and sets it on the engine. Bypass is never offered mid-session.
// It records nothing; ModeController.Set is the way a session changes mode.
func switchMode(cfg config.Config, pol *policy.Engine, arg string) error {
	switch policy.Mode(arg) {
	case policy.ModeDefault, policy.ModeAcceptEdits, policy.ModePlan, policy.ModeAuto:
	default:
		return fmt.Errorf("unknown mode %q", arg)
	}
	if _, err := cfg.Apply(config.Overrides{Mode: arg}); err != nil {
		return err
	}
	pol.Mode = policy.Mode(arg)
	return nil
}

// slashMode is /mode. Without an argument it says what the mode allows;
// auto needs a typed yes, and the managed configuration is asked first.
func slashMode(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	if len(args) == 0 {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: describeModes(e.st.appCfg, e.pol.Mode, e.modes.Offered(), e.st.maxTurns())})
		return false, nil
	}
	mode := policy.Mode(args[0])
	if mode == policy.ModeAuto && e.pol.Mode != policy.ModeAuto {
		// Refused over the ceiling before anyone is asked to agree to it.
		if err := switchMode(e.st.appCfg, policy.New(e.pol.Mode), string(mode)); err != nil {
			return false, err
		}
		answer := answered(e.ui.Dialog(ctx, ui.DialogSpec{
			Kind:  ui.DialogConfirm,
			Title: "Switch to auto mode?",
			Body:  []ui.Block{{Kind: ui.BlockNotice, Text: autoExplained}},
			Why:   "asked by /mode auto",
		}))
		if answer != ui.ChoiceYes {
			e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "mode stays " + string(e.pol.Mode)})
			return false, nil
		}
	}
	if err := e.modes.Set(ctx, mode, agent.ViaSlash); err != nil {
		return false, err
	}
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "mode: " + string(e.pol.Mode)})
	return false, nil
}

// answered is a dialog's answer, or "" when there was none: a question that
// ended unanswered is taken as no.
func answered(answer string, err error) string {
	if err != nil {
		return ""
	}
	return answer
}

// autoExplained is what auto mode approves on its own and what still asks.
// It is rules, not a judgment: nothing reviews a call's intent.
const autoExplained = "Auto approves, by rule and without asking: read-only tools, " +
	"and edit and write inside the workspace. Every approval is recorded with the rule " +
	"that made it.\nIt still asks for: commands (bash) no allow rule covers, anything an " +
	"ask rule names, destructive commands such as rm -rf, and reads that can send data " +
	"out. Deny rules still refuse in every mode."

// describeModes is what /mode shows with no argument.
func describeModes(cfg config.Config, current policy.Mode, offered []policy.Mode, maxTurns int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "current mode: %s\n", current)
	names := make([]string, len(offered))
	for i, m := range offered {
		names[i] = string(m)
	}
	fmt.Fprintf(&b, "Shift-Tab cycles: %s (auto only through /mode auto, bypass only at startup)", strings.Join(names, " → "))
	if current == policy.ModeAuto {
		b.WriteString("\n" + autoExplained)
	}
	if cfg.ManagedSets("permissions.mode") {
		fmt.Fprintf(&b, "\nthe managed configuration sets %s; only it or plan may be chosen", orDefault(cfg.Permissions.Mode, "default"))
	}
	if maxTurns > 0 {
		b.WriteString("\n" + turnLimitSummary(cfg, maxTurns))
	}
	return b.String()
}

// ModeController is the one way the session's permission mode changes: the
// startup flag, /mode, Shift-Tab and the answer to a proposed plan all go
// through it, and it holds the rules they share. A change is applied at a
// turn boundary: a caller mid-turn holds it until the turn ends.
type ModeController interface {
	// Offered is the Shift-Tab cycle, in order: default, accept-edits and
	// plan, less what the managed configuration leaves out or refuses. It
	// never holds auto or bypass.
	Offered() []policy.Mode
	// Cycle moves to the mode after the current one in Offered, or to the
	// first when the current one is not in it, and returns the new mode.
	Cycle() (policy.Mode, error)
	// Set changes to mode. via is how: agent.ViaFlag, ViaSlash, ViaShiftTab
	// or ViaPlanExit. Bypass comes only from the flag, Shift-Tab only reaches
	// what Offered holds, and a plan is never accepted into auto or bypass.
	// Each change is recorded as mode.changed.
	Set(ctx context.Context, mode policy.Mode, via string) error
}

// cliModes is the session's ModeController.
type cliModes struct {
	st  *cliState
	pol *policy.Engine
}

var _ ModeController = (*cliModes)(nil)

func (m *cliModes) Offered() []policy.Mode {
	var out []policy.Mode
	for _, name := range m.st.appCfg.ModeCycle() {
		if _, err := m.st.appCfg.Apply(config.Overrides{Mode: name}); err == nil {
			out = append(out, policy.Mode(name))
		}
	}
	return out
}

func (m *cliModes) Cycle() (policy.Mode, error) {
	offered := m.Offered()
	if len(offered) == 0 {
		return m.pol.Mode, errors.New("the managed configuration leaves no mode to cycle to")
	}
	next := offered[0]
	if i := slices.Index(offered, m.pol.Mode); i >= 0 {
		next = offered[(i+1)%len(offered)]
	}
	if next == m.pol.Mode {
		return next, nil
	}
	return next, m.Set(context.Background(), next, agent.ViaShiftTab)
}

func (m *cliModes) Set(_ context.Context, mode policy.Mode, via string) error {
	from := m.pol.Mode
	switch via {
	case agent.ViaFlag:
		// The flag alone may choose bypass; the managed configuration still refuses it.
		if !validMode(string(mode)) {
			return fmt.Errorf("unknown mode %q", mode)
		}
		if _, err := m.st.appCfg.Apply(config.Overrides{Mode: string(mode)}); err != nil {
			return err
		}
		m.pol.Mode = mode
		m.changed(from, mode, via)
		return nil
	case agent.ViaShiftTab:
		if !slices.Contains(m.Offered(), mode) {
			return fmt.Errorf("%s is not in the Shift-Tab cycle", mode)
		}
	case agent.ViaPlanExit:
		if mode == policy.ModeAuto || mode == policy.ModeBypass {
			return fmt.Errorf("a plan is never accepted into %s", mode)
		}
	case agent.ViaSlash:
	default:
		return fmt.Errorf("unknown way to change mode %q", via)
	}
	if mode == policy.ModeBypass {
		return errors.New("bypass is chosen only at startup, with -mode bypass")
	}
	if err := switchMode(m.st.appCfg, m.pol, string(mode)); err != nil {
		return err
	}
	m.changed(from, mode, via)
	return nil
}

// changed records a mode change that happened; staying put is not one.
func (m *cliModes) changed(from, to policy.Mode, via string) {
	if from == to {
		return
	}
	m.st.recordCLI(agent.EvModeChanged, agent.ModeChanged{
		From: string(from), To: string(to), By: agent.ByUser, Via: via,
	})
}

// pendingEvent is something the person did before the conversation had a
// record to hold it, such as a mode chosen before the first message.
type pendingEvent struct {
	typ     agent.EventType
	payload any
}

// recordCLI records one of the person's own actions in the conversation's
// record, or holds it until the next conversation opens. A failed write is
// reported: the action stands, and the person should know the record lacks it.
func (c *cliState) recordCLI(t agent.EventType, payload any) {
	if c.loop == nil || c.loop.Recorder == nil {
		c.pending = append(c.pending, pendingEvent{t, payload})
		return
	}
	if _, err := c.loop.Recorder.Record(t, agent.ActorUser, agent.Trusted, payload); err != nil {
		warnf("could not record %s: %v", t, err)
	}
}

// flushPending records what was held, once a conversation has opened.
func (c *cliState) flushPending() {
	held := c.pending
	c.pending = nil
	for _, p := range held {
		c.recordCLI(p.typ, p.payload)
	}
}

// dropPending forgets held events of type t, whose effect a new conversation
// does not carry.
func (c *cliState) dropPending(t agent.EventType) {
	c.pending = slices.DeleteFunc(c.pending, func(p pendingEvent) bool { return p.typ == t })
}

// lineAnswers reads a dialog's answers from the session's typed lines, until
// input ends. It answers the line surface the CLI uses until the terminal UI
// provides its own.
type lineAnswers struct {
	lines <-chan string
	ended <-chan struct{}
}

func (a lineAnswers) Await(ctx context.Context) (string, bool) {
	select {
	case line := <-a.lines:
		return line, true
	case <-a.ended:
		return "", false
	case <-ctx.Done():
		return "", false
	}
}

// turnsPerMessage is the allowance of turns the interactive CLI gives each
// message: the configured limit, per message, unless the managed
// configuration sets limits.max_turns, which then bounds the whole
// conversation as the organisation wrote it.
func turnsPerMessage(cfg config.Config, maxTurns int) int {
	if cfg.ManagedSets("limits.max_turns") || maxTurns <= 0 {
		return 0
	}
	return maxTurns
}

// turnLimitSummary says which turn limit applies, for /mode.
func turnLimitSummary(cfg config.Config, maxTurns int) string {
	if turnsPerMessage(cfg, maxTurns) > 0 {
		return fmt.Sprintf("turn limit: %d for each message", maxTurns)
	}
	return fmt.Sprintf("turn limit: %d for the whole conversation, set by the managed configuration", maxTurns)
}

// turnLimitNote is said when a run stops at the turn limit.
func turnLimitNote(cfg config.Config, maxTurns int) string {
	if turnsPerMessage(cfg, maxTurns) > 0 {
		return fmt.Sprintf("stopped after %d turns for this message; send \"continue\" to let it go on", maxTurns)
	}
	return fmt.Sprintf("the managed configuration allows %d turns for the whole conversation; /clear starts a new one", maxTurns)
}

// maxTurns is the conversation's turn limit as its loop was configured, or
// the configured limit before one has opened.
func (c *cliState) maxTurns() int {
	if n := c.turnLimit; n > 0 {
		return n
	}
	return c.appCfg.Limits.MaxTurns
}

// The answers to a proposed plan. Auto and bypass are never among them.
const (
	planAcceptEdits = "accept-edits"
	planAskEach     = "ask-each"
	planKeep        = "keep-planning"
)

// decidePlan puts a proposed plan to the person at the turn boundary and
// returns the message the conversation goes on with, or "". Accepting moves
// to accept-edits or default through the ModeController, never to auto.
func decidePlan(ctx context.Context, st *cliState, pol *policy.Engine, surface ui.Surface) string {
	if st.loop == nil {
		return ""
	}
	plan, ok := st.loop.TakePlan()
	if !ok {
		return ""
	}
	modes := &cliModes{st: st, pol: pol}
	var choices []ui.Choice
	for _, c := range []struct {
		id, label string
		key       rune
		mode      policy.Mode
	}{
		{planAcceptEdits, "Yes, and accept edits", 'a', policy.ModeAcceptEdits},
		{planAskEach, "Yes, ask before each change", 'y', policy.ModeDefault},
	} {
		if _, err := st.appCfg.Apply(config.Overrides{Mode: string(c.mode)}); err == nil {
			choices = append(choices, ui.Choice{ID: c.id, Label: c.label, Key: c.key})
		}
	}
	choices = append(choices, ui.Choice{ID: planKeep, Label: "No, keep planning (tell it what to change)"})
	answer, err := surface.Dialog(ctx, ui.DialogSpec{
		Kind:    ui.DialogChoice,
		Title:   "Proceed with this plan?",
		Body:    []ui.Block{{Kind: ui.BlockMarkdown, Text: plan.Text}},
		Choices: choices,
		Default: planKeep,
		Why:     "proposed by the agent with exit_plan; it changes nothing until you answer",
	})
	to := map[string]policy.Mode{planAcceptEdits: policy.ModeAcceptEdits, planAskEach: policy.ModeDefault}[answer]
	if err != nil || to == "" {
		st.recordCLI(agent.EvPlanDecided, agent.PlanDecided{Decision: agent.PlanKeepPlanning})
		surface.Append(ui.Block{Kind: ui.BlockNotice, Text: "still in plan mode; say what to change"})
		return ""
	}
	if err := modes.Set(ctx, to, agent.ViaPlanExit); err != nil {
		st.recordCLI(agent.EvPlanDecided, agent.PlanDecided{Decision: agent.PlanKeepPlanning})
		surface.Append(ui.Block{Kind: ui.BlockError, Text: "the mode was not changed, so planning goes on: " + err.Error()})
		return ""
	}
	st.recordCLI(agent.EvPlanDecided, agent.PlanDecided{Decision: agent.PlanAccepted, ToMode: string(to)})
	surface.Append(ui.Block{Kind: ui.BlockNotice, Text: "mode: " + string(to)})
	return fmt.Sprintf("The plan is approved (mode %s). Carry it out.", to)
}

// viaCarried marks a mode.changed that restates, in a new conversation's
// record, a mode chosen in an earlier conversation of the session.
const viaCarried = "carried"

// carryState holds, for the next conversation, the mode and added folders it
// inherits, in place of the changes that made them, so its record says each once.
func (c *cliState) carryState() {
	c.pending = slices.DeleteFunc(c.pending, func(p pendingEvent) bool {
		return p.typ == agent.EvModeChanged || p.typ == agent.EvWorkspaceDirAdded
	})
	var carried []pendingEvent
	if start := policy.Mode(orDefault(c.appCfg.Permissions.Mode, "default")); c.pol != nil && c.pol.Mode != start {
		carried = append(carried, pendingEvent{agent.EvModeChanged, agent.ModeChanged{
			From: string(start), To: string(c.pol.Mode), By: agent.ByUser, Via: viaCarried}})
	}
	for _, d := range c.addedDirs {
		carried = append(carried, pendingEvent{agent.EvWorkspaceDirAdded, d})
	}
	c.pending = append(carried, c.pending...)
}
