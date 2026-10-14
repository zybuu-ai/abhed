package sandbox

import (
	"path/filepath"
	"testing"
)

// Each tier names the HOME its commands get, and whether the host can read
// it, so the policy can read the shell start-up files there.
func TestCommandHomeByTier(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	tmp := t.TempDir()
	for name, c := range map[string]struct {
		sb      Sandbox
		dir     string
		visible bool
	}{
		"process":         {&Process{}, "/home/someone", true},
		"none":            {&None{}, "/home/someone", true},
		"fence":           {&Fence{tmp: tmp}, filepath.Join(tmp, "home"), true},
		"fence unstarted": {&Fence{}, "", false},
		"container":       {&Container{}, "/tmp", false},
	} {
		dir, visible := CommandHome(c.sb)
		if dir != c.dir || visible != c.visible {
			t.Errorf("%s: %q %v, want %q %v", name, dir, visible, c.dir, c.visible)
		}
	}
}
