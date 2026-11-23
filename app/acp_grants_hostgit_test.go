package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/policy"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// Host git's ask offers "always for this repository's remote" alone; the
// grant then answers fetch, pull and ls-remote to that repository's remote
// as approved, not push, a forced push, another repository or the remote
// changed, and no exact or folder grant answers host git's ask.
func TestACPHostGitGrant(t *testing.T) {
	var seen []string
	c, asked := answeringConn(t, func(params json.RawMessage) any {
		var p struct {
			Options []struct {
				OptionID string `json:"optionId"`
				Name     string `json:"name"`
			} `json:"options"`
		}
		_ = json.Unmarshal(params, &p)
		for _, o := range p.Options {
			seen = append(seen, o.Name)
		}
		for _, o := range p.Options {
			if strings.HasPrefix(o.OptionID, "always-family:") {
				return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": o.OptionID}}
			}
		}
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
	})
	s := &acpSession{id: "s1", cwd: "/ws", cancel: func() {}, always: map[string]string{}}
	host := abhed.Decision{Decision: policy.Ask, Step: hostgit.StepHostGit, Scope: "/ws\x00aaaa"}
	// An exact grant kept before does not answer host git's ask.
	s.addGrant(bashGrant{Command: "git fetch", Session: true})
	ctx, answer := agent.ExpectAnswer(context.Background())
	if ok, err := c.askEditor(ctx, s, "bash", json.RawMessage(`{"command":"git fetch"}`), host); err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if asked.Load() != 1 {
		t.Fatalf("asked %d times", asked.Load())
	}
	labels := strings.Join(seen, " | ")
	if !strings.Contains(labels, "Always allow git fetch, pull and ls-remote from this remote of this repository") || strings.Contains(labels, "exactly") {
		t.Fatalf("options %s", labels)
	}
	if answer.Granted != "bash(git fetch/pull *) to /ws's remote aaaa" {
		t.Fatalf("answer %+v", answer)
	}
	for line, want := range map[string]bool{"git pull --ff-only": false, "git ls-remote origin": false, "git push origin main": true,
		"git push --force origin main": true} {
		n := asked.Load()
		_, _ = c.askEditor(context.Background(), s, "bash", json.RawMessage(`{"command":"`+line+`"}`), host)
		if got := asked.Load() != n; got != want {
			t.Errorf("%s: asked %v, want %v", line, got, want)
		}
	}
	// Another repository, or this one with its remote changed, is asked again.
	for _, bind := range []string{"/other\x00aaaa", "/ws\x00bbbb"} {
		n := asked.Load()
		_, _ = c.askEditor(context.Background(), s, "bash", json.RawMessage(`{"command":"git fetch"}`),
			abhed.Decision{Decision: policy.Ask, Step: hostgit.StepHostGit, Scope: bind})
		if asked.Load() != n+1 {
			t.Errorf("the grant answered for %q", bind)
		}
	}
	// Nor does it answer a default ask for the same line.
	if _, ok := s.grantFor("git pull --ff-only", ""); ok {
		t.Error("a repository grant answered a default ask")
	}
	// Asked each time: never answered by a grant.
	n := asked.Load()
	_, _ = c.askEditor(context.Background(), s, "bash", json.RawMessage(`{"command":"git fetch"}`), abhed.Decision{Decision: policy.Ask, Step: hostgit.StepHostGitEach})
	if asked.Load() != n+1 {
		t.Fatal("a grant answered an ask made each time")
	}
}
