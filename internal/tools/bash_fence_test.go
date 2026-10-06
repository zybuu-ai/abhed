package tools

import (
	"strings"
	"testing"
)

// Under the fence the model is told where commands may write and that unix
// sockets are refused; elsewhere it is not.
func TestBashDescribesTheFence(t *testing.T) {
	if d := (Bash{Isolation: Isolation{Tier: "fence"}}).Description(); !strings.Contains(d, "private temp folder") || !strings.Contains(d, "unix sockets") {
		t.Fatalf("fence: %s", d)
	}
	if d := (Bash{Isolation: Isolation{Tier: "process"}}).Description(); strings.Contains(d, "fence") {
		t.Fatalf("process: %s", d)
	}
}
