package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

// A layer's rules decide as configured ones do, in every mode, and each
// decision names the layer; a configured rule names none.
func TestLayerRulesNameTheirLayer(t *testing.T) {
	e := New(ModeBypass)
	if err := e.AddDeny("bash(rm -rf*)"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddLayer("workspace pay", []string{"bash(curl*)"}, []string{"bash(git push*)"}, []string{"bash(make*)"}); err != nil {
		t.Fatal(err)
	}
	cmd := func(c string) json.RawMessage { b, _ := json.Marshal(map[string]string{"command": c}); return b }
	for _, c := range []struct {
		command, decision, step, rule, layer string
	}{
		{"curl https://x", "deny", "deny", "bash(curl*)", "workspace pay"},
		{"git push origin main", "ask", "ask", "bash(git push*)", "workspace pay"},
		{"rm -rf build", "deny", "deny", "bash(rm -rf*)", ""},
	} {
		r := e.Evaluate("bash", true, cmd(c.command))
		if string(r.Decision) != c.decision || r.Step != c.step || r.Rule != c.rule || r.Layer != c.layer {
			t.Errorf("%s: %+v, want %s at %s by %s of %q", c.command, r, c.decision, c.step, c.rule, c.layer)
		}
		if c.layer != "" && !strings.Contains(r.Reason, c.layer+" rule ") {
			t.Errorf("%s: the reason does not name the layer: %s", c.command, r.Reason)
		}
	}
	e.Mode = ModeDefault
	if r := e.Evaluate("bash", true, cmd("make test")); r.Decision != Allow || r.Layer != "workspace pay" {
		t.Errorf("a layer's allow rule: %+v", r)
	}
	for _, r := range e.Allow {
		if r.String() == "bash(make*)" && r.Layer() != "workspace pay" {
			t.Errorf("the rule does not carry its layer: %q", r.Layer())
		}
	}
}

// A layer with one rule that does not parse adds none of its rules, and a
// layer needs a name.
func TestLayerIsAllOrNothing(t *testing.T) {
	e := New(ModeDefault)
	if err := e.AddLayer("workspace pay", []string{"bash(curl*)"}, []string{"bash(git push"}, nil); err == nil {
		t.Fatal("a layer with a broken rule was accepted")
	}
	if len(e.Deny)+len(e.Ask)+len(e.Allow) != 0 {
		t.Fatalf("a refused layer left rules behind: %v %v %v", e.Deny, e.Ask, e.Allow)
	}
	if err := e.AddLayer("  ", []string{"bash(curl*)"}, nil, nil); err == nil {
		t.Fatal("a layer without a name was accepted")
	}
}
