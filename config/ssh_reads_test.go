package config

import (
	"slices"
	"strings"
	"testing"
)

// ssh.read_commands load from the person's files, are checked as one simple
// command each, cannot come from an untrusted workspace, and are set aside
// when the managed file sets the permissions without naming its own; a
// managed list replaces the person's.
func TestSSHReadCommands(t *testing.T) {
	_, ws := trustHome(t, `{}`, "")
	body := []byte(`{"ssh":{"read_commands":["uptime","systemctl status *"]}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true, Settings: body, SettingsName: "ci.json"})
	if err != nil || !slices.Equal(cfg.SSH.ReadCommands, []string{"uptime", "systemctl status *"}) {
		t.Fatalf("load: %v %v", cfg.SSH.ReadCommands, err)
	}
	for _, bad := range []string{`["uptime; id"]`, `["sudo *"]`, `["rm -rf /tmp/x"]`, `[""]`} {
		_, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: []byte(`{"ssh":{"read_commands":` + bad + `}}`), SettingsName: "bad.json"})
		if err == nil || !strings.Contains(err.Error(), "ssh.read_commands") {
			t.Errorf("read_commands %s loaded: %v", bad, err)
		}
	}

	_, ws = trustHome(t, `{}`, `{"ssh":{"read_commands":["cat *"]}}`)
	if cfg, err = LoadWith(ws, LoadOptions{Quiet: true}); err != nil || len(cfg.SSH.ReadCommands) != 0 {
		t.Fatalf("an untrusted workspace declared a read: %v %v", cfg.SSH.ReadCommands, err)
	}

	withManaged(t, `{"permissions":{"deny":["bash(rm *)"]}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: body, SettingsName: "ci.json"})
	if err != nil || len(cfg.SSH.ReadCommands) != 0 {
		t.Fatalf("under managed permissions: %v %v", cfg.SSH.ReadCommands, err)
	}
	if !slices.ContainsFunc(cfg.SetAside, func(k SetAsideKey) bool {
		return k.Key == "ssh.read_commands" && k.Value == "uptime" && k.File == "ci.json"
	}) {
		t.Fatalf("the dropped read was not named: %+v", cfg.SetAside)
	}

	withManaged(t, `{"permissions":{"deny":["bash(rm *)"]},"ssh":{"read_commands":["uptime"]}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: body, SettingsName: "ci.json"})
	if err != nil || !slices.Equal(cfg.SSH.ReadCommands, []string{"uptime"}) {
		t.Fatalf("the managed list: %v %v", cfg.SSH.ReadCommands, err)
	}
	withManaged(t, `{"ssh":{"read_commands":[]}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: body, SettingsName: "ci.json"})
	if err != nil || len(cfg.SSH.ReadCommands) != 0 {
		t.Fatalf("a managed empty list did not lock it: %v %v", cfg.SSH.ReadCommands, err)
	}
}
