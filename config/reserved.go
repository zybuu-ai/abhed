package config

import "fmt"

// notYetInEffect are the settings this version reads but does not act on
// yet. A file that sets one is warned, and abhed doctor reports it, so an
// administrator never believes a control is in force when it is not.
//
// The track that wires a setting deletes its entry here in the same change,
// and its test that the setting takes effect replaces the warning.
var notYetInEffect = []string{
	"cli.mode_cycle",        // Shift-Tab and the ModeController (governance)
	"statusline",            // the statusline command (status)
	"memory.auto",           // auto memory (input and memory)
	"record.dir",            // the local record (record and sessions)
	"record.retention_days", // record pruning (record and sessions)
	"hooks.disabled",        // hooks (governance)
}

// NotYetInEffect lists the settings the configuration's files made that this
// version does not act on yet, as dotted paths.
func (c Config) NotYetInEffect() []string {
	var out []string
	for _, k := range notYetInEffect {
		if c.Sets(k) {
			out = append(out, k)
		}
	}
	return out
}

// warnNotYetInEffect writes each such setting once per process.
func warnNotYetInEffect(c Config) {
	warnedMu.Lock()
	defer warnedMu.Unlock()
	for _, k := range c.NotYetInEffect() {
		if id := "reserved\x00" + k; !warned[id] {
			warned[id] = true
			fmt.Fprintf(warnOut, "abhed: warning: %s\n", NotYetInEffectMessage(k))
		}
	}
}

// NotYetInEffectMessage is what is said about one such setting.
func NotYetInEffectMessage(key string) string {
	return fmt.Sprintf("%s is set but not yet in effect in this version; it is accepted so the file stays valid", key)
}
