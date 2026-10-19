package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeUserFile writes ~/.abhed/config.json under the test's HOME.
func writeUserFile(t *testing.T, body string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".abhed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The person turns the OCSF file on and picks its version; where it goes is
// the managed file's to say.
func TestOCSFUserSettingsAndManagedPath(t *testing.T) {
	withManaged(t, `{}`)
	writeUserFile(t, `{"record": {"ocsf": {"enabled": true, "version": "1.5.0", "path": "/tmp/elsewhere.jsonl", "locked": true}}}`)
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.Record.OCSF
	if !o.Enabled || o.Version != "1.5.0" {
		t.Fatalf("the person's own settings were not kept: %+v", o)
	}
	if o.Path != "" || o.Locked {
		t.Fatalf("a user file moved or locked the OCSF file: %+v", o)
	}
	var aside []string
	for _, k := range cfg.SetAside {
		aside = append(aside, k.Key)
	}
	if got := strings.Join(aside, ","); !strings.Contains(got, "record.ocsf.path") || !strings.Contains(got, "record.ocsf.locked") {
		t.Fatalf("set aside: %s", got)
	}
}

// A managed lock makes the managed section the whole of it: the person can
// neither turn the file off nor change its version.
func TestOCSFManagedLock(t *testing.T) {
	withManaged(t, `{"record": {"ocsf": {"enabled": true, "locked": true, "path": "/var/log/abhed/ocsf.jsonl"}}}`)
	writeUserFile(t, `{"record": {"ocsf": {"enabled": false, "version": "1.1.0", "keep": 1}}}`)
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	want := OCSFConfig{Enabled: true, Locked: true, Path: "/var/log/abhed/ocsf.jsonl"}
	if cfg.Record.OCSF != want {
		t.Fatalf("got %+v, want %+v", cfg.Record.OCSF, want)
	}
}

// Without the lock, a managed value still wins where it is set and the
// person's own fill the rest.
func TestOCSFManagedWithoutLock(t *testing.T) {
	withManaged(t, `{"record": {"ocsf": {"enabled": true}}}`)
	writeUserFile(t, `{"record": {"ocsf": {"enabled": false, "version": "1.3.0"}}}`)
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if o := cfg.Record.OCSF; !o.Enabled || o.Version != "1.3.0" {
		t.Fatalf("got %+v", o)
	}
}

func TestOCSFValidate(t *testing.T) {
	for _, bad := range []OCSFConfig{{Version: "1.4.0"}, {Version: "2.0"}, {MaxMB: -1}, {Keep: -2}} {
		c := Default()
		c.Record.OCSF = bad
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "record.ocsf") {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}
