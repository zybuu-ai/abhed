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

// The read-only classifier judges commands on the hosts in ssh.hosts that
// load, and on no other. ssh.listed_reads_only turns it off; a workspace may
// only turn it off; and a managed file that sets the permissions or any ssh
// key turns it off unless it sets ssh.listed_reads_only itself.
func TestSSHReadOnlyHosts(t *testing.T) {
	hosts := `"hosts":[{"name":"app1","addr":"127.0.0.1:22","user":"ops"},{"name":"half","addr":"10.0.0.9"}]`
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
	// A host with no user fails to load, so ssh_connect could otherwise claim its name.
	if on("half") {
		t.Error("a host that does not load is trusted")
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

	for _, body := range []string{
		`{"permissions":{"deny":["bash(rm *)"]}}`,
		`{"ssh":{"read_commands":["uptime"]}}`,
		`{"ssh":{"hosts":[{"name":"app1","addr":"127.0.0.1:22","user":"ops"}]}}`,
		`{"ssh":{"transfers":false}}`,
	} {
		withManaged(t, body)
		if f := load(`{"ssh":{"enabled":true,` + hosts + `}}`).SSHReadOnlyHosts(); f != nil {
			t.Errorf("managed %s left the classifier on", body)
		}
	}
	withManaged(t, `{"permissions":{"deny":["bash(rm *)"]},"ssh":{"listed_reads_only":false}}`)
	if f := load(`{"ssh":{"enabled":true,` + hosts + `}}`).SSHReadOnlyHosts(); f == nil || !f("app1") {
		t.Error("the managed file's own listed_reads_only:false did not keep it on")
	}
	withManaged(t, `{"permissions":{"deny":["bash(rm *)"]},"ssh":{"listed_reads_only":true}}`)
	if f := load(`{"ssh":{"enabled":true,"listed_reads_only":false,` + hosts + `}}`).SSHReadOnlyHosts(); f != nil {
		t.Error("a user file overrode the managed listed_reads_only:true")
	}
}

// ssh.read_contents is off by default, set by the person's own file, never
// by a workspace or an org policy, and under a managed file that sets the
// permissions or an ssh key only by the managed file itself.
func TestSSHReadContents(t *testing.T) {
	hosts := `"hosts":[{"name":"app1","addr":"127.0.0.1:22","user":"ops"}]`
	_, ws := trustHome(t, `{"ssh":{"enabled":true,`+hosts+`}}`, `{"ssh":{"read_contents":true}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil || cfg.SSHReadContents() {
		t.Fatalf("a workspace turned read_contents on: %v", err)
	}
	if !orgRefused("ssh.read_contents") {
		t.Error("an org policy may set read_contents")
	}
	_, ws = trustHome(t, `{"ssh":{"enabled":true,"read_contents":true,`+hosts+`}}`, "")
	if cfg, err = LoadWith(ws, LoadOptions{Quiet: true}); err != nil || !cfg.SSHReadContents() {
		t.Fatalf("the person's own read_contents: %v", err)
	}
	withManaged(t, `{"ssh":{"read_commands":["uptime"]}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: []byte(`{"ssh":{"enabled":true,"read_contents":true,` + hosts + `}}`), SettingsName: "ci.json"})
	if err != nil || cfg.SSHReadContents() {
		t.Fatalf("under a managed ssh key, the person's read_contents held: %v", err)
	}
	withManaged(t, `{"ssh":{"read_contents":true,"listed_reads_only":false}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: []byte(`{"ssh":{"enabled":true,` + hosts + `}}`), SettingsName: "ci.json"})
	if err != nil || !cfg.SSHReadContents() || cfg.SSHReadOnlyHosts() == nil {
		t.Fatalf("the managed file's own read_contents: %v", err)
	}
	withManaged(t, `{"ssh":{"read_contents":false}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: []byte(`{"ssh":{"enabled":true,"read_contents":true,` + hosts + `}}`), SettingsName: "ci.json"})
	if err != nil || cfg.SSHReadContents() {
		t.Fatalf("a user file overrode the managed read_contents:false: %v", err)
	}
}

// orgRefused reports a key the org policy may never set.
func orgRefused(key string) bool {
	r, ok := orgRuleFor(key)
	return ok && r.kind == orgRefuse
}

// ssh.max_timeout_seconds loads from 0 to a day; anything else stops the
// load, so no value can wrap or quietly mean the default.
func TestSSHMaxTimeoutBounds(t *testing.T) {
	trustHome(t, `{}`, "")
	for _, v := range []string{"-1", "86401", "9223372037", "20000000000"} {
		_, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: []byte(`{"ssh":{"max_timeout_seconds":` + v + `}}`), SettingsName: "ci.json"})
		if err == nil || !strings.Contains(err.Error(), "ssh.max_timeout_seconds") {
			t.Errorf("%s loaded: %v", v, err)
		}
	}
	for _, v := range []string{"0", "30", "86400"} {
		if _, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: []byte(`{"ssh":{"max_timeout_seconds":` + v + `}}`), SettingsName: "ci.json"}); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
}

// A ** entry for a program that prints files is warned of where
// ssh.read_contents is off, since it then runs only calls that print none.
func TestDoubleStarContentEntryWarns(t *testing.T) {
	c := Default()
	c.SSH.Enabled = true
	c.SSH.ReadCommands = []string{"cat **", "ls **", "tail -n * /var/log/app/app.log", "head -n 5 *", "getent *", "cat /var/log/app/*"}
	w := strings.Join(c.Warnings(), "\n")
	for _, e := range []string{`"cat **"`, `"head -n 5 *"`, `"getent *"`} {
		if !strings.Contains(w, e) {
			t.Errorf("no warning for %s: %s", e, w)
		}
	}
	if strings.Contains(w, `"ls **"`) || strings.Contains(w, "tail -n") || strings.Contains(w, `"cat /var/log/app/*"`) {
		t.Fatalf("warnings: %s", w)
	}
	c.SSH.ReadContents = true
	if w := strings.Join(c.Warnings(), "\n"); strings.Contains(w, "cat **") {
		t.Fatalf("warned with read_contents on: %s", w)
	}
}
