package config

import (
	"errors"
	"slices"
	"testing"
)

func TestSupervisorIsOnByDefaultWithTheRules(t *testing.T) {
	_, ws := trustHome(t, "", "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Monitor.Enabled || cfg.Monitor.JudgeName() != "rules" {
		t.Fatalf("%+v", cfg.Monitor)
	}
}

func TestMonitorValidation(t *testing.T) {
	for name, m := range map[string]MonitorConfig{
		"judge":       {Judge: "llm"},
		"unavailable": {OnUnavailable: "allow"},
		"no endpoint": {Enabled: true, Judge: "model", Model: "m"},
		"no model":    {Enabled: true, Judge: "model", Endpoint: "http://127.0.0.1:8000/v1"},
		"scheme":      {Endpoint: "file:///tmp/x"},
		"userinfo":    {Endpoint: "https://u:p@judge.example/v1"},
		"negative":    {TimeoutMS: -1},
	} {
		if m.validate() == nil {
			t.Errorf("%s: accepted %+v", name, m)
		}
	}
	ok := MonitorConfig{Enabled: true, Judge: "model", Endpoint: "http://127.0.0.1:8000/v1", Model: "guard"}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
}

// A workspace that is not trusted may only tighten the supervisor.
func TestWorkspaceMonitorOnlyTightens(t *testing.T) {
	_, ws := trustHome(t, `{"monitor":{"enabled":false,"max_per_turn":5}}`,
		`{"monitor":{"enabled":true,"on_unavailable":"deny","max_per_turn":10,"honeytokens":["AKIAHONEYTOKEN0001"]}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Monitor
	if !m.Enabled || m.OnUnavailable != "deny" || m.MaxPerTurn != 10 || !slices.Contains(m.Honeytokens, "AKIAHONEYTOKEN0001") {
		t.Fatalf("%+v", m)
	}
	_, ws = trustHome(t, "", `{"monitor":{"enabled":false,"judge":"model","endpoint":"https://judge.example/v1","model":"m","max_per_turn":2}}`)
	cfg, err = LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg.Monitor; !m.Enabled || m.Judge != "" || m.Endpoint != "" || m.MaxPerTurn != 0 {
		t.Fatalf("an untrusted workspace loosened or redirected the supervisor: %+v", m)
	}
	for _, k := range []string{"monitor.enabled", "monitor.judge", "monitor.endpoint", "monitor.max_per_turn"} {
		if !ignored(cfg.Workspace, k) {
			t.Errorf("%s not reported ignored: %+v", k, cfg.Workspace.Ignored)
		}
	}
}

// The managed file binds what it sets; what the person sets below it stays
// only where it is stricter.
func TestManagedMonitor(t *testing.T) {
	withManaged(t, `{"monitor":{"enabled":true,"judge":"rules","on_unavailable":"ask","max_per_turn":5,"honeytokens":["MANAGEDHONEY0001"]}}`)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, home, `{"monitor":{"enabled":false,"judge":"model","endpoint":"http://127.0.0.1:9/v1","model":"m","on_unavailable":"deny","max_per_turn":9,"honeytokens":["USERHONEY000001"]}}`)
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Monitor
	if !m.Enabled || m.Judge != "rules" || m.OnUnavailable != "deny" || m.MaxPerTurn != 9 ||
		!slices.Contains(m.Honeytokens, "MANAGEDHONEY0001") || !slices.Contains(m.Honeytokens, "USERHONEY000001") {
		t.Fatalf("%+v", m)
	}
	if _, err := cfg.Apply(Overrides{Supervisor: "off"}); !isManaged(err) {
		t.Fatalf("turning a managed supervisor off: %v", err)
	}
	if _, err := cfg.Apply(Overrides{Supervisor: "model"}); !isManaged(err) {
		t.Fatalf("changing a managed judge: %v", err)
	}
	if c, err := cfg.Apply(Overrides{Supervisor: "rules"}); err != nil || c.Monitor.JudgeName() != "rules" {
		t.Fatalf("%v %+v", err, c.Monitor)
	}
}

// A managed false binds unless the person turned it on themselves.
func TestManagedMonitorOffAndAPersonsOwnOn(t *testing.T) {
	withManaged(t, `{"monitor":{"enabled":false}}`)
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Monitor.Enabled {
		t.Fatal("a managed false did not bind over the default")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, home, `{"monitor":{"enabled":true}}`)
	if cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true}); err != nil || !cfg.Monitor.Enabled {
		t.Fatalf("a person's own supervisor was turned off: %v %+v", err, cfg.Monitor)
	}
}

func TestApplySupervisorWithoutManaged(t *testing.T) {
	c := Default()
	if c2, err := c.Apply(Overrides{Supervisor: "off"}); err != nil || c2.Monitor.Enabled {
		t.Fatalf("%v %+v", err, c2.Monitor)
	}
	if _, err := c.Apply(Overrides{Supervisor: "model"}); err == nil {
		t.Fatal("a model judge with no endpoint was accepted")
	}
	if _, err := c.Apply(Overrides{Supervisor: "maybe"}); err == nil {
		t.Fatal("an unknown supervisor was accepted")
	}
}

func isManaged(err error) bool {
	var me *ManagedError
	return errors.As(err, &me)
}
