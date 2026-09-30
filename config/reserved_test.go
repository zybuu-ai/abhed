package config

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// A file that sets a control this version does not act on yet is warned,
// from the user's file and from the managed one alike: an administrator must
// not believe hooks are off or retention is enforced when neither is.
func TestSettingsNotYetInEffectAreWarned(t *testing.T) {
	for _, c := range []struct{ name, key, body string }{
		{"hooks", "hooks.disabled", `{"hooks":{"disabled":true}}`},
		{"retention", "record.retention_days", `{"record":{"retention_days":90}}`},
		{"record dir", "record.dir", `{"record":{"dir":"/srv/records"}}`},
		{"mode cycle", "cli.mode_cycle", `{"cli":{"mode_cycle":["plan"]}}`},
		{"auto memory", "memory.auto", `{"memory":{"auto":false}}`},
		{"import depth", "memory.import_depth", `{"memory":{"import_depth":2}}`},
		{"commands", "commands.dirs", `{"commands":{"dirs":["c"]}}`},
		{"rules", "rules.dirs", `{"rules":{"dirs":["r"]}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			warnOut = &out
			t.Cleanup(func() { warnOut = os.Stderr })
			warnedMu.Lock()
			clear(warned)
			warnedMu.Unlock()
			withManaged(t, c.body)
			cfg, err := Load(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.NotYetInEffect(); len(got) != 1 || got[0] != c.key {
				t.Fatalf("NotYetInEffect = %v, want [%s]", got, c.key)
			}
			if !strings.Contains(out.String(), c.key+" is set but not yet in effect in this version") {
				t.Fatalf("not warned:\n%s", out.String())
			}
		})
	}
	var out bytes.Buffer
	warnOut = &out
	// statusline is in effect: the CLI runs it.
	withManaged(t, `{"permissions":{"mode":"default"},"statusline":{"command":"s.sh"}}`)
	if cfg, _ := Load(t.TempDir()); len(cfg.NotYetInEffect()) != 0 || strings.Contains(out.String(), "not yet in effect") {
		t.Fatalf("a file with none of them was warned: %v %q", cfg.NotYetInEffect(), out.String())
	}
}
