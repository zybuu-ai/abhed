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

// A family grant covers no line that hands the program another program,
// script, settings file, project or output path, nor one naming a path
// outside the folder or a module@version; flags that only select or report,
// such as -p 4 for go test and -p crate for cargo, are covered.
func TestFamilyGrantsRefuseWhatHandsOverControl(t *testing.T) {
	for _, c := range []struct {
		family, cmd string
	}{
		{"go vet", "go vet -vettool=/tmp/evil ./..."},
		{"go vet", "go vet -vettool x ./..."},
		{"npm test", "npm test --prefix /home/u/other"},
		{"npm test", "npm test --prefix other"},
		{"cargo test", "cargo test --manifest-path /home/u/other/Cargo.toml"},
		{"cargo test", "cargo test --manifest-path other/Cargo.toml"},
		{"gradle test", "gradle test --init-script /tmp/x.gradle"},
		{"gradle test", "gradle test -I x.gradle"},
		{"mvn test", "mvn test -s /tmp/settings.xml"},
		{"mvn test", "mvn test -f other/pom.xml"},
		{"go run", "go run example.com/m@latest"},
		{"go build", "go build -o ~/.zshrc ."},
		{"go build", "go build -o bin/x ."},
		{"go test", "go test -exec x ./..."},
		{"go test", "go test ../other/..."},
		{"go test", "go test /abs/pkg"},
		{"go test", "go test -C ../x ./..."},
		{"python3 -m pytest", "python3 -m pytest -p evil_plugin"},
		{"python3 -m pytest", "python3 -m pytest -c /tmp/pytest.ini"},
		{"npm run lint", "npm run lint --script-shell /bin/evil"},
		{"go test", "go test ./... -- -x"},
	} {
		if (bashGrant{Family: c.family}).covers(c.cmd, "/ws") {
			t.Errorf("%q covered by the %s grant", c.cmd, c.family)
		}
	}
	for cmd, fam := range map[string]string{
		"go test -p 4 ./...":            "go test",
		"cargo test -p crate":           "cargo test",
		"cargo test -- --nocapture":     "cargo test",
		"npm run lint -- --fix":         "npm run lint",
		"mvn test -Dtest=FooTest":       "mvn test",
		"gradle test --tests Foo":       "gradle test",
		"python3 -m pytest -q -k slow":  "python3 -m pytest",
		"go test -run=TestX -count=1 .": "go test",
		"go run ./cmd/tool":             "go run",
	} {
		if got := familyOf(cmd); got != fam {
			t.Errorf("%q: %q, want %q", cmd, got, fam)
		}
	}
}

// Round 2: -mod that fetches, a go package outside the module, a further
// Maven goal or Gradle task, a path behind = or :, and npm run env are
// covered by no family grant; a line naming a path outside the folder or a
// module@version is offered no scope at all.
func TestGrantsRoundTwo(t *testing.T) {
	for _, c := range []struct{ family, cmd string }{
		{"go test", "go test -mod=mod example.com/evil/pkg"},
		{"go run", "go run -mod=mod example.com/evil/cmd"},
		{"go test", "go test -mod mod ./..."},
		{"go test", "go test -mod=mod ./..."},
		{"go test", "go test example.com/x"},
		{"go build", "go build github.com/evil/tool"},
		{"mvn test", "mvn test deploy"},
		{"mvn test", "mvn test org.example:evil-maven-plugin:1.0:pwn"},
		{"mvn test", "mvn test exec:exec"},
		{"mvn test", "mvn test -pl :core"},
		{"gradle test", "gradle test publish"},
		{"pytest", "pytest --cov-report=html:/abs/out"},
		{"python3 -m pytest", "python3 -m pytest --cov-report=html:../out"},
		{"mvn test", "mvn test -Dtest=a=/x"},
		{"npm test", "npm test -- X=/etc/passwd"},
		{"npm run env", "npm run env"},
	} {
		if (bashGrant{Family: c.family}).covers(c.cmd, "/ws") || familyOf(c.cmd) != "" {
			t.Errorf("%q covered (family %q)", c.cmd, familyOf(c.cmd))
		}
	}
	for cmd, fam := range map[string]string{
		"go test -mod=readonly ./...": "go test",
		"go build -mod=vendor .":      "go build",
		"go list -m all":              "go list",
		"mvn test -pl core -am":       "mvn test",
		"mvn test -T 4 -q":            "mvn test",
		"gradle test -x lint":         "gradle test",
		"gradle test --tests Foo":     "gradle test",
		"pytest --cov-report=html":    "pytest",
	} {
		if got := familyOf(cmd); got != fam {
			t.Errorf("%q: %q, want %q", cmd, got, fam)
		}
	}
	for _, cmd := range []string{"go run example.com/m@latest", "go test /abs/pkg", "go test ../x", "make -C /tmp", "npm test -- X=/etc/passwd"} {
		if opts := bashGrantOptions(cmd, "/ws", "b"); len(opts) != 0 {
			t.Errorf("%q was offered %d scopes", cmd, len(opts))
		}
		if (bashGrant{Command: cmd}).covers(cmd, "/ws") {
			t.Errorf("an exact grant covers %q", cmd)
		}
	}
}
