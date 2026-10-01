package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// planEnv is recordedEnv in plan mode with a plan waiting for the person.
func planEnv(t *testing.T, cfg config.Config, answers ...string) (*cmdEnv, *scriptedSurface, func() []agent.Event) {
	t.Helper()
	env, surface, events := recordedEnv(t, cfg, policy.ModePlan, answers...)
	env.st.loop.EnablePlanExit()
	tool, _ := env.st.loop.Tools.Get("exit_plan")
	if res := tool.Run(context.Background(), env.sess, []byte(`{"plan":"1. edit main.go"}`)); res.IsError {
		t.Fatal(res.Content)
	}
	return env, surface, events
}

func planDecisions(t *testing.T, evs []agent.Event) []string {
	var out []string
	for _, p := range eventsOf[agent.PlanDecided](t, evs, agent.EvPlanDecided) {
		out = append(out, p.Decision+">"+p.ToMode)
	}
	return out
}

// The person's answer decides: accepting moves to accept-edits or default
// through the ModeController, and the conversation goes on; anything else,
// the default included, keeps planning. Auto is never offered.
func TestPlanDecision(t *testing.T) {
	for _, c := range []struct {
		name      string
		answers   []string
		mode      policy.Mode
		decisions []string
		changes   []string
	}{
		{"accept edits", []string{planAcceptEdits}, policy.ModeAcceptEdits,
			[]string{"accept>accept-edits"}, []string{"plan>accept-edits/plan-exit"}},
		{"ask each", []string{planAskEach}, policy.ModeDefault,
			[]string{"accept>default"}, []string{"plan>default/plan-exit"}},
		{"keep", []string{planKeep}, policy.ModePlan, []string{"keep-planning>"}, nil},
		{"enter", []string{""}, policy.ModePlan, []string{"keep-planning>"}, nil},
		{"no answer", nil, policy.ModePlan, []string{"keep-planning>"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			env, surface, events := planEnv(t, config.Default(), c.answers...)
			next := decidePlan(context.Background(), env.st, env.pol, surface)
			if env.pol.Mode != c.mode {
				t.Fatalf("mode %s, want %s", env.pol.Mode, c.mode)
			}
			if (next != "") != (c.mode != policy.ModePlan) {
				t.Fatalf("goes on with %q", next)
			}
			if got := planDecisions(t, events()); !slices.Equal(got, c.decisions) {
				t.Fatalf("decisions %v", got)
			}
			if got := modeChanges(t, events()); !slices.Equal(got, c.changes) {
				t.Fatalf("mode changes %v", got)
			}
			d := surface.asked[0]
			if d.Default != planKeep {
				t.Fatalf("default %q", d.Default)
			}
			for _, ch := range d.Choices {
				if strings.Contains(strings.ToLower(ch.Label+ch.ID), "auto") || strings.Contains(ch.ID, "bypass") {
					t.Fatalf("offered %+v", ch)
				}
			}
			if decidePlan(context.Background(), env.st, env.pol, surface) != "" || len(surface.asked) != 1 {
				t.Fatal("the same plan was put to the person twice")
			}
		})
	}
}

// A managed mode leaves out the answers it would refuse.
func TestPlanDecisionHonoursAManagedMode(t *testing.T) {
	cfg := config.Default()
	cfg.Managed, cfg.ManagedKeys, cfg.Permissions.Mode = true, []string{"permissions.mode"}, "default"
	env, surface, _ := planEnv(t, cfg, planAcceptEdits)
	decidePlan(context.Background(), env.st, env.pol, surface)
	var ids []string
	for _, ch := range surface.asked[0].Choices {
		ids = append(ids, ch.ID)
	}
	if !slices.Equal(ids, []string{planAskEach, planKeep}) || env.pol.Mode != policy.ModePlan {
		t.Fatalf("offered %v, mode %s", ids, env.pol.Mode)
	}
}

// Through the CLI: the agent proposes a plan in plan mode, the person picks
// "ask before each change", the mode is default, and the next edit asks.
func TestCLIPlanExit(t *testing.T) {
	var ws atomic.Value
	c := startCLIWith(t, func(w io.Writer, n int, _ string) {
		switch n {
		case 1:
			sseCall(w, n, "exit_plan", `{"plan":"1. create notes.txt"}`)
		case 2:
			sseCall(w, n, "write", `{"path":`+strconv.Quote(filepath.Join(ws.Load().(string), "notes.txt"))+`,"content":"x"}`)
		default:
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
	})
	ws.Store(c.ws)
	c.command("/mode plan", "mode: plan")
	fmt.Fprintln(c.stdin, "plan a notes file")
	c.waitFor(func(out string) bool {
		return strings.Contains(out, "Proceed with this plan?") && strings.Contains(out, "answer 1-3")
	}, "the plan card")
	c.command("2", "mode: default")
	// Default mode: the edit the plan leads to is put to the person.
	c.waitFor(func(out string) bool {
		i := strings.LastIndex(out, "needs approval")
		return i >= 0 && strings.Contains(out[i:], "answer 1-")
	}, "the edit to be asked about")
	c.command(c.declineNumber(), " in / ")
	if _, err := os.Stat(filepath.Join(c.ws, "notes.txt")); err == nil {
		t.Fatal("a rejected write was made")
	}
	evs := c.export()
	if got := planDecisions(t, evs); !slices.Equal(got, []string{"accept>default"}) {
		t.Fatalf("decisions %v", got)
	}
	var changes []string
	for _, mc := range eventsOf[agent.ModeChanged](t, evs, agent.EvModeChanged) {
		changes = append(changes, mc.From+">"+mc.To+"/"+mc.Via)
	}
	if !slices.Equal(changes, []string{"default>plan/slash", "plan>default/plan-exit"}) {
		t.Fatalf("mode changes %v", changes)
	}
	var types []agent.EventType
	for _, e := range evs {
		types = append(types, e.Type)
	}
	proposed, decided := slices.Index(types, agent.EvPlanProposed), slices.Index(types, agent.EvPlanDecided)
	if proposed < 0 || decided < proposed {
		t.Fatalf("events out of order: %v", types)
	}
}

// An acceptance the mode change then refuses is recorded as keep-planning,
// never as an accepted plan whose mode did not change.
func TestPlanAcceptedIntoARefusedModeKeepsPlanning(t *testing.T) {
	cfg := config.Default()
	cfg.Managed, cfg.ManagedKeys, cfg.Permissions.Mode = true, []string{"permissions.mode"}, "default"
	env, surface, events := planEnv(t, cfg, planAcceptEdits)
	if next := decidePlan(context.Background(), env.st, env.pol, surface); next != "" || env.pol.Mode != policy.ModePlan {
		t.Fatalf("went on with %q in mode %s", next, env.pol.Mode)
	}
	if got := planDecisions(t, events()); !slices.Equal(got, []string{"keep-planning>"}) {
		t.Fatalf("recorded %v", got)
	}
}
