package config

import (
	"strings"
	"testing"
)

// A negative or absurd sign-in length is refused at load, naming the key.
func TestSessionHoursIsBounded(t *testing.T) {
	for _, h := range []string{"-1", "-24", "8761"} {
		withManaged(t, `{"auth":{"mode":"local","session_hours":`+h+`}}`)
		_, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true})
		if err == nil || !strings.Contains(err.Error(), "auth.session_hours is "+h) {
			t.Errorf("session_hours %s: %v", h, err)
		}
	}
	for _, h := range []string{"0", "1", "8760"} {
		withManaged(t, `{"auth":{"mode":"local","session_hours":`+h+`}}`)
		if _, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true}); err != nil {
			t.Errorf("session_hours %s refused: %v", h, err)
		}
	}
}
