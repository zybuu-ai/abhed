package app

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func modesFor(cfg config.Config, start policy.Mode) (*cliModes, *policy.Engine) {
	pol := policy.New(start)
	return &cliModes{st: &cliState{appCfg: cfg}, pol: pol}, pol
}

// Shift-Tab never reaches auto or bypass, however often it is pressed and
// wherever it starts, and the managed configuration can only take modes out.
func TestModeCycleNeverReachesAutoOrBypass(t *testing.T) {
	pinned := config.Default()
	pinned.Managed, pinned.ManagedKeys, pinned.Permissions.Mode = true, []string{"permissions.mode"}, "default"
	narrowed := config.Default()
	narrowed.CLI.ModeCycle = []string{"plan", "default"}
	planOnly := config.Default()
	planOnly.CLI.ModeCycle = []string{"plan"}

	for _, c := range []struct {
		name    string
		cfg     config.Config
		start   policy.Mode
		offered []policy.Mode
		first   policy.Mode
	}{
		{"default cycle", config.Default(), policy.ModeDefault,
			[]policy.Mode{policy.ModeDefault, policy.ModeAcceptEdits, policy.ModePlan}, policy.ModeAcceptEdits},
		{"from auto", config.Default(), policy.ModeAuto,
			[]policy.Mode{policy.ModeDefault, policy.ModeAcceptEdits, policy.ModePlan}, policy.ModeDefault},
		{"from bypass", config.Default(), policy.ModeBypass,
			[]policy.Mode{policy.ModeDefault, policy.ModeAcceptEdits, policy.ModePlan}, policy.ModeDefault},
		{"managed cycle leaves accept-edits out", narrowed, policy.ModeDefault,
			[]policy.Mode{policy.ModeDefault, policy.ModePlan}, policy.ModePlan},
		{"managed mode refuses accept-edits", pinned, policy.ModeDefault,
			[]policy.Mode{policy.ModeDefault, policy.ModePlan}, policy.ModePlan},
		{"one mode stays put", planOnly, policy.ModePlan, []policy.Mode{policy.ModePlan}, policy.ModePlan},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, pol := modesFor(c.cfg, c.start)
			if got := m.Offered(); !slices.Equal(got, c.offered) {
				t.Fatalf("offered %v, want %v", got, c.offered)
			}
			for i := range 20 {
				got, err := m.Cycle()
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 && got != c.first {
					t.Fatalf("first press went to %s, want %s", got, c.first)
				}
				if got != pol.Mode || !slices.Contains(c.offered, got) || got == policy.ModeAuto || got == policy.ModeBypass {
					t.Fatalf("press %d reached %s (engine %s)", i+1, got, pol.Mode)
				}
			}
		})
	}
}

// Each way in keeps its own limits: bypass only from the flag and never under
// a managed file, Shift-Tab only within the cycle, a plan never into auto.
func TestModeSetRules(t *testing.T) {
	managed := config.Default()
	managed.Managed = true
	for _, c := range []struct {
		name string
		cfg  config.Config
		mode policy.Mode
		via  string
		ok   bool
	}{
		{"slash auto", config.Default(), policy.ModeAuto, agent.ViaSlash, true},
		{"slash bypass", config.Default(), policy.ModeBypass, agent.ViaSlash, false},
		{"slash unknown", config.Default(), "yolo", agent.ViaSlash, false},
		{"shift-tab auto", config.Default(), policy.ModeAuto, agent.ViaShiftTab, false},
		{"shift-tab plan", config.Default(), policy.ModePlan, agent.ViaShiftTab, true},
		{"plan-exit auto", config.Default(), policy.ModeAuto, agent.ViaPlanExit, false},
		{"plan-exit bypass", config.Default(), policy.ModeBypass, agent.ViaPlanExit, false},
		{"plan-exit accept-edits", config.Default(), policy.ModeAcceptEdits, agent.ViaPlanExit, true},
		{"flag bypass", config.Default(), policy.ModeBypass, agent.ViaFlag, true},
		{"flag bypass under a managed file", managed, policy.ModeBypass, agent.ViaFlag, false},
		{"unknown way", config.Default(), policy.ModePlan, "hook", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, pol := modesFor(c.cfg, policy.ModeDefault)
			err := m.Set(context.Background(), c.mode, c.via)
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok %v", err, c.ok)
			}
			want := policy.ModeDefault
			if c.ok {
				want = c.mode
			}
			if pol.Mode != want {
				t.Fatalf("mode is %s, want %s", pol.Mode, want)
			}
		})
	}
}

// scriptedSurface answers dialogs from a list, in order, and keeps what was
// shown. Out of answers, a dialog is unanswered, as input ending is.
type scriptedSurface struct {
	answers []string
	asked   []ui.DialogSpec
	shown   []ui.Block
	// during runs while a dialog waits, as another process might act then.
	during func()
}

var _ ui.Surface = (*scriptedSurface)(nil)

func (f *scriptedSurface) Append(b ui.Block) { f.shown = append(f.shown, b) }
func (f *scriptedSurface) Dialog(_ context.Context, d ui.DialogSpec) (string, error) {
	n, err := d.Normalized()
	if err != nil {
		return "", err
	}
	f.asked = append(f.asked, n)
	if f.during != nil {
		f.during()
	}
	if len(f.answers) == 0 {
		return "", ui.ErrNoAnswer
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	if a == "" {
		return n.Default, nil
	}
	return a, nil
}
func (f *scriptedSurface) Pick(context.Context, ui.PickSpec) (string, error) {
	return "", ui.ErrNoAnswer
}
func (f *scriptedSurface) Panel(_ context.Context, p ui.PanelSpec) error {
	f.shown = append(f.shown, p.Body...)
	return nil
}
func (f *scriptedSurface) SetStatus(ui.StatusModel) {}
func (f *scriptedSurface) Notify(t ui.Toast) {
	f.shown = append(f.shown, ui.Block{Kind: ui.BlockNotice, Text: t.Text})
}

func (f *scriptedSurface) text() string {
	var b strings.Builder
	for _, s := range f.shown {
		b.WriteString(s.Text + "\n")
		for _, r := range s.Rows {
			b.WriteString(strings.Join(r, " | ") + "\n")
		}
	}
	return b.String()
}

// recordedEnv is a command environment over an open conversation whose
// record is kept in a memory store, answering dialogs with answers.
func recordedEnv(t *testing.T, cfg config.Config, start policy.Mode, answers ...string) (*cmdEnv, *scriptedSurface, func() []agent.Event) {
	t.Helper()
	store := agent.NewMemStore()
	pol := policy.New(start)
	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := &cliState{appCfg: cfg, store: store, sess: sess, workspace: sess.Root, pol: pol}
	st.loop = &agent.Loop{Recorder: agent.NewRecorder(store, "s1", ""), Policy: pol, Session: sess}
	st.sessionID = "s1"
	surface := &scriptedSurface{answers: answers}
	st.surface = surface
	env := &cmdEnv{ui: surface, st: st, pol: pol, sess: sess, modes: &cliModes{st: st, pol: pol}}
	return env, surface, func() []agent.Event {
		evs, _ := store.Events("s1")
		return evs
	}
}

// modeChanges lists the recorded mode changes as from>to/via.
func modeChanges(t *testing.T, evs []agent.Event) []string {
	t.Helper()
	var out []string
	for _, ev := range evs {
		if ev.Type != agent.EvModeChanged {
			continue
		}
		var mc agent.ModeChanged
		if err := json.Unmarshal(ev.Payload, &mc); err != nil {
			t.Fatal(err)
		}
		if ev.Actor != agent.ActorUser || mc.By != agent.ByUser {
			t.Fatalf("mode change credited to %s/%s", ev.Actor, mc.By)
		}
		out = append(out, mc.From+">"+mc.To+"/"+mc.Via)
	}
	return out
}

// Every way the mode changes is recorded, with how; a refused change and a
// change to the mode already in force are not.
func TestModeChangesAreRecorded(t *testing.T) {
	env, _, events := recordedEnv(t, config.Default(), policy.ModeDefault)
	m := env.modes
	if _, err := m.Cycle(); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(context.Background(), policy.ModePlan, agent.ViaSlash); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(context.Background(), policy.ModePlan, agent.ViaSlash); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(context.Background(), policy.ModeAuto, agent.ViaPlanExit); err == nil {
		t.Fatal("a plan was accepted into auto")
	}
	if err := m.Set(context.Background(), policy.ModeDefault, agent.ViaPlanExit); err != nil {
		t.Fatal(err)
	}
	want := []string{"default>accept-edits/shift-tab", "accept-edits>plan/slash", "plan>default/plan-exit"}
	if got := modeChanges(t, events()); !slices.Equal(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
}

// A change made before the conversation has a record is held and recorded
// when it opens, ahead of anything else the conversation does.
func TestModeChangeBeforeTheFirstMessageIsRecordedWhenItOpens(t *testing.T) {
	env, _, events := recordedEnv(t, config.Default(), policy.ModeDefault)
	loop := env.st.loop
	env.st.loop = nil
	if err := env.modes.Set(context.Background(), policy.ModePlan, agent.ViaSlash); err != nil {
		t.Fatal(err)
	}
	if len(events()) != 0 || len(env.st.pending) != 1 {
		t.Fatalf("recorded before there was a conversation: %d events, %d held", len(events()), len(env.st.pending))
	}
	env.st.loop = loop
	env.st.flushPending()
	if got := modeChanges(t, events()); !slices.Equal(got, []string{"default>plan/slash"}) {
		t.Fatalf("recorded %v", got)
	}
	if len(env.st.pending) != 0 {
		t.Fatal("held events were kept after they were recorded")
	}
}

// /mode auto asks first, with no as the default; no answer, a no or an
// Enter leaves the mode where it was, and a managed ceiling refuses it
// before anyone is asked.
func TestModeAutoNeedsAYes(t *testing.T) {
	for _, c := range []struct {
		name    string
		answers []string
		want    policy.Mode
	}{
		{"no answer", nil, policy.ModeDefault},
		{"enter", []string{""}, policy.ModeDefault},
		{"no", []string{ui.ChoiceNo}, policy.ModeDefault},
		{"yes", []string{ui.ChoiceYes}, policy.ModeAuto},
	} {
		t.Run(c.name, func(t *testing.T) {
			env, surface, events := recordedEnv(t, config.Default(), policy.ModeDefault, c.answers...)
			if _, err := slashMode(context.Background(), env, []string{"auto"}); err != nil {
				t.Fatal(err)
			}
			if len(surface.asked) != 1 || surface.asked[0].Default != ui.ChoiceNo {
				t.Fatalf("asked %+v", surface.asked)
			}
			if env.pol.Mode != c.want {
				t.Fatalf("mode %s, want %s", env.pol.Mode, c.want)
			}
			if changed := modeChanges(t, events()); (c.want == policy.ModeAuto) != (len(changed) == 1) {
				t.Fatalf("recorded %v", changed)
			}
		})
	}
	pinned := config.Default()
	pinned.Managed, pinned.ManagedKeys, pinned.Permissions.Mode = true, []string{"permissions.mode"}, "default"
	env, surface, events := recordedEnv(t, pinned, policy.ModeDefault, ui.ChoiceYes)
	if _, err := slashMode(context.Background(), env, []string{"auto"}); err == nil || env.pol.Mode != policy.ModeDefault {
		t.Fatalf("auto over a managed ceiling: %v, mode %s", err, env.pol.Mode)
	}
	if len(surface.asked) != 0 || len(events()) != 0 {
		t.Fatalf("asked %d times and recorded %d events for a refused mode", len(surface.asked), len(events()))
	}
}

// /mode with no argument says what auto approves, by rule, and names no
// classifier; the cycle it shows never holds auto or bypass.
func TestModeWithoutArgumentExplains(t *testing.T) {
	env, surface, _ := recordedEnv(t, config.Default(), policy.ModeAuto)
	if _, err := slashMode(context.Background(), env, nil); err != nil {
		t.Fatal(err)
	}
	out := surface.text()
	for _, want := range []string{"current mode: auto", "by rule", "destructive commands", "default → accept-edits → plan"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "classif") {
		t.Fatalf("auto mode described as a classifier:\n%s", out)
	}
}
