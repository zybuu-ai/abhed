package app

import (
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A served session's bash reads start-up files from its own fence's home,
// not the server's: empty until that fence is made.
func TestServedFenceSessionUsesItsOwnHome(t *testing.T) {
	shared := tools.Bash{Home: func() (string, bool) { return "/server/fence/home", true }}
	b, closeFn := fenceSessionBash(config.Config{}, t.TempDir())("s1", nil, shared)
	defer func() { _ = closeFn() }()
	if b.Home == nil {
		t.Fatal("no home")
	}
	if dir, visible := b.Home(); dir != "" || !visible {
		t.Fatalf("home before the session's fence is made: %q %v", dir, visible)
	}
}
