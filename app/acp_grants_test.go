package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// A family is the program and subcommand, the script for npm run and the
// module for python -m; a line with a code flag, a chain, an expansion, an
// assignment or a path has none.
func TestFamilyOf(t *testing.T) {
	for cmd, want := range map[string]string{
		"go test ./...":             "go test",
		"go test -run TestX ./pkg":  "go test",
		"npm run lint":              "npm run lint",
		"npm run lint -- --fix":     "npm run lint",
		"npm test":                  "npm test",
		"python3 -m pytest -q":      "python3 -m pytest",
		"cargo test --release":      "cargo test",
		"pytest tests/":             "pytest",
		"go test -exec sh ./...":    "",
		"go build -toolexec x .":    "",
		"cargo test --config x=1":   "",
		"go test ./... && rm -rf x": "",
		"go test $(pwd)":            "",
		"CGO_ENABLED=0 go test":     "",
		"./go test":                 "",
		"go generate ./...":         "",
		"npm install":               "",
		"npm run":                   "",
		"python3 -m http.server":    "",
		"node -e 1":                 "",
		"rm -rf build":              "",
	} {
		if got := familyOf(cmd); got != want {
			t.Errorf("%q: %q, want %q", cmd, got, want)
		}
	}
	g := bashGrant{Family: "go test", Folder: "/ws/svc"}
	if !g.covers("go test ./x", "/ws/svc/orders") || g.covers("go test ./x", "/ws/other") || g.covers("go vet ./x", "/ws/svc") {
		t.Fatal("folder family grant")
	}
	if !(bashGrant{Command: "make check"}).covers("make check", "") || (bashGrant{Command: "make check"}).covers("make check2", "") {
		t.Fatal("exact grant")
	}
}

// The permission request offers the bash scopes with clear labels; a chosen
// family grant answers the next ask of that family, recorded, and never an
// ask a rule, the destructive check or a screen made.
func TestACPBashGrants(t *testing.T) {
	pick := func(prefix string) func(json.RawMessage) any {
		return func(params json.RawMessage) any {
			var p struct {
				Options []struct {
					OptionID string `json:"optionId"`
					Name     string `json:"name"`
				} `json:"options"`
			}
			_ = json.Unmarshal(params, &p)
			for _, o := range p.Options {
				if strings.HasPrefix(o.OptionID, prefix) {
					return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": o.OptionID}}
				}
			}
			return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
		}
	}
	var seen []string
	c, asked := answeringConn(t, func(params json.RawMessage) any {
		var p struct {
			Options []struct {
				Name string `json:"name"`
			} `json:"options"`
		}
		_ = json.Unmarshal(params, &p)
		for _, o := range p.Options {
			seen = append(seen, o.Name)
		}
		return pick("always-family:")(params)
	})
	s := &acpSession{id: "s1", cwd: "/ws", cancel: func() {}, always: map[string]string{}}
	d := abhed.Decision{Decision: policy.Ask, Step: "default"}
	ctx, answer := agent.ExpectAnswer(context.Background())
	if ok, err := c.askEditor(ctx, s, "bash", json.RawMessage(`{"command":"go test ./..."}`), d); err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	labels := strings.Join(seen, " | ")
	for _, want := range []string{"Allow `go test ./...` for this session", "Always allow exactly `go test ./...`", "Always allow `go test …` in ws", "Always allow `go test …` commands"} {
		if !strings.Contains(labels, want) {
			t.Errorf("no option %q in %s", want, labels)
		}
	}
	if answer.Granted != "bash(go test *)" || len(s.grants) != 1 || s.grants[0].Family != "go test" || s.grants[0].Session {
		t.Fatalf("answer %+v grants %+v", answer, s.grants)
	}
	// The next go test is answered by the grant; go vet asks.
	before := asked.Load()
	if ok, _ := c.askEditor(context.Background(), s, "bash", json.RawMessage(`{"command":"go test -run X ./pkg"}`), d); !ok || asked.Load() != before {
		t.Fatalf("the grant did not answer: asked %d", asked.Load()-before)
	}
	if _, _ = c.askEditor(context.Background(), s, "bash", json.RawMessage(`{"command":"go test -exec sh ./..."}`), d); asked.Load() != before+1 {
		t.Fatal("a code flag was covered by the family grant")
	}
	// Steps other than the default ask are never answered by a grant.
	for _, step := range []string{"ask", "destructive", "screen", "supervisor"} {
		n := asked.Load()
		_, _ = c.askEditor(context.Background(), s, "bash", json.RawMessage(`{"command":"go test ./..."}`), abhed.Decision{Decision: policy.Ask, Step: step})
		if asked.Load() != n+1 {
			t.Errorf("step %s was answered by a grant", step)
		}
	}
}

// A grant for the session alone is recorded as such and not restored.
func TestACPSessionGrantIsNotRestored(t *testing.T) {
	s := &acpSession{}
	for _, g := range []bashGrant{{Command: "make check", Session: true}, {Family: "go test", Folder: "/ws"}} {
		b, _ := json.Marshal(g.grantRecord())
		if !s.restoreGrant(b) {
			t.Fatalf("%+v not read back", g)
		}
	}
	if len(s.grants) != 1 || s.grants[0].Family != "go test" || s.grants[0].Folder != "/ws" {
		t.Fatalf("restored %+v", s.grants)
	}
	if s.restoreGrant(json.RawMessage(`{"scope":"bash(ls *)","by":"user"}`)) {
		t.Fatal("a policy scope was taken for a bash grant")
	}
}
