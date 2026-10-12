package config

import "fmt"

// notYetInEffect are the settings this version reads but does not act on
// yet. A file that sets one is warned, and abhed doctor reports it, so an
// administrator never believes a control is in force when it is not.
//
// The track that wires a setting deletes its entry here in the same change,
// and its test that the setting takes effect replaces the warning.
var notYetInEffect = []string{}

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

// warnNotYetInEffect warns of each such setting.
func warnNotYetInEffect(w warnSink, c Config) {
	for _, k := range c.NotYetInEffect() {
		w.emit("reserved\x00"+k, NotYetInEffectMessage(k))
	}
}

// NotYetInEffectMessage is what is said about one such setting.
func NotYetInEffectMessage(key string) string {
	return fmt.Sprintf("%s is set but not yet in effect in this version; it is accepted so the file stays valid", key)
}
