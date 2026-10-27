//go:build !darwin

package loopport

// Ephemeral is the range the system gives a listener that asks for no
// port; outside macOS the rule does not apply, and this is the IANA range.
func Ephemeral() (lo, hi int) { return 49152, 65535 }
