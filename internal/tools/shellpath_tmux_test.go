package tools

import "testing"

// tmux runs a command by its alias, its full name or a unique prefix of it.
func TestTmuxResolvesAsTmuxDoes(t *testing.T) {
	for name, want := range map[string]string{"ls": "list-sessions", "send": "send-keys", "send-k": "send-keys",
		"send-ke": "send-keys", "set-b": "set-buffer", "paste-b": "paste-buffer", "pipe-p": "pipe-pane",
		"run-s": "run-shell", "new-w": "new-window", "display-p": "", "show": "show-options", "set": "set-option",
		"new": "new-session", "kill-ser": "kill-server", "sendk": "", "s": "", "list-s": "list-sessions"} {
		if got := tmuxResolve(name); got != want {
			t.Errorf("tmux %s: %q, want %q", name, got, want)
		}
	}
}
