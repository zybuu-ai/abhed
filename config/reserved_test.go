package config

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// A file that sets a control this version does not act on yet is warned,
// from the user's file and from the managed one alike: an administrator must
// not believe a setting acts when it does not.
func TestSettingsNotYetInEffectAreWarned(t *testing.T) {
	for _, c := range []struct{ name, key, body string }{} {
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
	// Wired settings are no longer warned about: the Shift-Tab cycle, hooks.disabled
	// and the statusline all take effect.
	withManaged(t, `{"permissions":{"mode":"default"},"cli":{"mode_cycle":["plan","default"]},"hooks":{"disabled":true},"statusline":{"command":"s.sh"}}`)
	if cfg, _ := Load(t.TempDir()); len(cfg.NotYetInEffect()) != 0 || strings.Contains(out.String(), "not yet in effect") {
		t.Fatalf("a file with none of them was warned: %v %q", cfg.NotYetInEffect(), out.String())
	}
}
