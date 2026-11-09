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

// A wildcard entry for a program off the read-only list, or in a form of a
// listed one that changes the host, stops the load and says to declare the
// exact command.
func TestSSHReadCommandsWildcardsOnlyForKnownReads(t *testing.T) {
	for _, bad := range []string{`["systemctl *"]`, `["socat *"]`, `["find /var/log *"]`, `["journalctl --vacuum-size=*"]`} {
		_, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: []byte(`{"ssh":{"read_commands":` + bad + `}}`), SettingsName: "bad.json"})
		if err == nil || !strings.Contains(err.Error(), "declare the exact command") {
			t.Errorf("%s loaded: %v", bad, err)
		}
	}
	good := []byte(`{"ssh":{"read_commands":["systemctl status *","journalctl -u * -n 50","nginx -V"]}}`)
	if _, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: good, SettingsName: "ok.json"}); err == nil || !strings.Contains(err.Error(), "nginx") {
		t.Errorf("an exact entry for nginx, which loads modules a config names, loaded: %v", err)
	}
	good = []byte(`{"ssh":{"read_commands":["systemctl status *","journalctl -u * -n 50","uname -a"]}}`)
	if _, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: good, SettingsName: "ok.json"}); err != nil {
		t.Errorf("read entries did not load: %v", err)
	}
}

// The read-only classifier judges commands on the hosts in ssh.hosts and on
// no other. ssh.listed_reads_only turns it off; a workspace may only turn it
// off; and a managed file that sets the permissions turns it off unless it
// sets ssh.listed_reads_only itself.
func TestSSHReadOnlyHosts(t *testing.T) {
	hosts := `"hosts":[{"name":"app1","addr":"127.0.0.1:22","user":"ops"}]`
	load := func(settings string) Config {
		t.Helper()
		cfg, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: []byte(settings), SettingsName: "ci.json"})
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	trustHome(t, `{}`, "")
	on := load(`{"ssh":{"enabled":true,` + hosts + `}}`).SSHReadOnlyHosts()
	if on == nil || !on("app1") || on("vm-added") {
		t.Fatalf("declared hosts: on=%t", on != nil)
	}
	if f := load(`{"ssh":{"enabled":true,"listed_reads_only":true,` + hosts + `}}`).SSHReadOnlyHosts(); f != nil {
		t.Error("listed_reads_only left the classifier on")
	}
	if f := load(`{"ssh":{"enabled":false,` + hosts + `}}`).SSHReadOnlyHosts(); f != nil {
		t.Error("ssh off left the classifier on")
	}

	// A workspace may turn it off, never back on.
	_, ws := trustHome(t, `{"ssh":{"enabled":true,`+hosts+`}}`, `{"ssh":{"listed_reads_only":true}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil || !cfg.SSH.ListedReadsOnly || cfg.SSHReadOnlyHosts() != nil {
		t.Fatalf("an untrusted workspace could not turn it off: %v %v", cfg.SSH.ListedReadsOnly, err)
	}
	_, ws = trustHome(t, `{"ssh":{"enabled":true,"listed_reads_only":true,`+hosts+`}}`, `{"ssh":{"listed_reads_only":false}}`)
	if cfg, err = LoadWith(ws, LoadOptions{Quiet: true}); err != nil || !cfg.SSH.ListedReadsOnly {
		t.Fatalf("an untrusted workspace turned it back on: %v %v", cfg.SSH.ListedReadsOnly, err)
	}

	withManaged(t, `{"permissions":{"deny":["bash(rm *)"]}}`)
	if f := load(`{"ssh":{"enabled":true,` + hosts + `}}`).SSHReadOnlyHosts(); f != nil {
		t.Error("managed permissions left the classifier on")
	}
	withManaged(t, `{"permissions":{"deny":["bash(rm *)"]},"ssh":{"listed_reads_only":false}}`)
	if f := load(`{"ssh":{"enabled":true,` + hosts + `}}`).SSHReadOnlyHosts(); f == nil || !f("app1") {
		t.Error("the managed file's own listed_reads_only:false did not keep it on")
	}
}
