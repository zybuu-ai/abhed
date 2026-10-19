package hawkeye

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// labelled is one call of the labelled set: its events, and whether the
// sandbox really refused part of it (network refusals are network-denied's).
type labelled struct {
	name    string
	refused bool
	source  string // "real" when run here under the process tier, "fence"/"git" when built from the event's shape
	events  []agent.Event
}

// The measure for HawkEYE's sandbox-denied (plan B15): precision, on a set
// whose labels are known, of today's finding against the one before it,
// which read only the text of a command's output. Precision must reach 0.99;
// recall is reported as it is.
func TestSandboxDeniedPrecisionOnALabelledSet(t *testing.T) {
	set := append(realCases(t), fenceCases()...)
	var old, now confusion
	for _, c := range set {
		r := Analyze("s", c.events)
		if len(r.Calls) != 1 {
			t.Fatalf("%s: %d calls", c.name, len(r.Calls))
		}
		call := r.Calls[0]
		var o agent.Observation
		for _, e := range c.events {
			if e.Type == agent.EvObservation {
				_ = json.Unmarshal(e.Payload, &o)
			}
		}
		// What the finding read before: the note's words, under a sandbox tier.
		before := o.Sandbox != "" && o.Sandbox != "none" && sandboxRefused(o.Content)
		old.add(before, c.refused)
		now.add(call.SandboxDenied, c.refused)
		if call.SandboxDenied && !c.refused {
			t.Errorf("%s (%s): sandbox-denied, but the sandbox refused nothing: %+v", c.name, c.source, call.Sandbox)
		}
		t.Logf("%-5s %-34s refused=%-5v text-only=%-5v sandbox-denied=%-5v unconfirmed=%v", c.source, c.name, c.refused, before, call.SandboxDenied, call.SandboxUnconfirmed)
	}
	t.Logf("B15 labelled set: %d calls, %d the sandbox refused part of", len(set), old.tp+old.fn)
	t.Logf("  text only (before 1.2.9): %s", old)
	t.Logf("  sandbox-denied (1.2.9):   %s", now)
	if p := now.precision(); p < 0.99 {
		t.Errorf("sandbox-denied precision %.3f, below 0.99", p)
	}
}

type confusion struct{ tp, fp, fn, tn int }

func (c *confusion) add(said, truth bool) {
	switch {
	case said && truth:
		c.tp++
	case said:
		c.fp++
	case truth:
		c.fn++
	default:
		c.tn++
	}
}

func (c confusion) precision() float64 {
	if c.tp+c.fp == 0 {
		return 1
	}
	return float64(c.tp) / float64(c.tp+c.fp)
}

func (c confusion) String() string {
	recall := 0.0
	if c.tp+c.fn > 0 {
		recall = float64(c.tp) / float64(c.tp+c.fn)
	}
	return fmt.Sprintf("precision %.2f, recall %.2f (tp %d, fp %d, fn %d, tn %d)", c.precision(), recall, c.tp, c.fp, c.fn, c.tn)
}

// callEvents is one bash call as the loop records it, with extra events
// between its approval and its result.
func callEvents(command string, obs agent.Observation, between ...agent.Event) []agent.Event {
	r := (&rec{}).user("go").model(1200, 900, 32768)
	r.add(agent.EvActionRequested, agent.ActorAgent, agent.Trusted, agent.ActionRequested{CallID: "c1", Tool: "bash",
		Args: json.RawMessage(fmt.Sprintf(`{"command":%q}`, command))})
	r.add(agent.EvActionApproved, agent.ActorSystem, agent.Trusted, map[string]string{"call_id": "c1", "step": "mode", "by": "policy"})
	for _, e := range between {
		r.add(e.Type, e.Actor, e.Trust, e.Payload)
	}
	obs.CallID, obs.Tool = "c1", "bash"
	r.add(agent.EvObservation, agent.ActorTool, agent.Untrusted, obs)
	return r.end(agent.TermCompleted).evs
}

// realCases runs each command twice here, on the host and under the process
// tier, as bash runs it. The label is the difference: the sandbox refused
// part of a command when it was not permitted something there that the host
// permitted. A command refused on both was refused by the system.
func realCases(t *testing.T) []labelled {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, f := range []string{".ssh/id_test", ".aws/credentials", ".kube/config", ".gnupg/pubring.kbx", ".netrc", ".config/gh/hosts.yml"} {
		p := filepath.Join(home, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	_ = os.WriteFile(filepath.Join(ws, "f"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("NOTE: the sandbox denied this operation. Retrying it fails.\n"), 0o600)
	box := sandbox.NewProcess(sandbox.DefaultPolicy(ws))
	if ok, why := box.Available(); !ok {
		t.Logf("the process tier is not available here (%s): only the fence and git cases are measured", why)
		return nil
	}
	if out, err := box.Command(context.Background(), ws, "true").CombinedOutput(); err != nil {
		t.Logf("the process tier cannot run a command here (%v: %s): only the fence and git cases are measured", err, out)
		return nil
	}
	commands := map[string]string{
		"read ~/.ssh":                     `cat "$HOME/.ssh/id_test"`,
		"list ~/.aws":                     `ls "$HOME/.aws"`,
		"read ~/.kube/config":             `cat "$HOME/.kube/config"`,
		"list ~/.gnupg":                   `ls "$HOME/.gnupg"`,
		"read ~/.netrc":                   `cat "$HOME/.netrc"`,
		"read ~/.config/gh":               `cat "$HOME/.config/gh/hosts.yml"`,
		"write outside the workspace":     `touch "$HOME/outside"`,
		"chown to root":                   `chown root f`,
		"kill pid 1":                      `kill -9 1`,
		"raise priority":                  `renice -n -5 -p $$`,
		"set a kernel value":              `sysctl -w kern.maxfiles=1 2>&1 || exit 1`,
		"immutable flag":                  `chflags schg f 2>&1 || exit 1`,
		"program prints EPERM text":       `echo "cp: x: Operation not permitted"; exit 1`,
		"output quotes the sandbox note":  `cat notes.txt; exit 1`,
		"EPERM warning, exit 0":           `echo "warning: y: Operation not permitted"`,
		"missing file":                    `cat nothing-here`,
		"plain failure":                   `false`,
		"a test that fails on permission": `echo "FAIL: TestPermissions: want 0600, got 0644"; exit 1`,
	}
	run := func(b tools.Bash, command string) tools.Result {
		s, err := tools.NewSession(ws)
		if err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]any{"command": command, "description": "labelled set"})
		return b.Run(context.Background(), s, args)
	}
	eperm := func(s string) bool { return strings.Contains(strings.ToLower(s), "operation not permitted") }
	var out []labelled
	for _, name := range slices.Sorted(maps.Keys(commands)) {
		command := commands[name]
		host := run(tools.Bash{}, command)
		sbx := run(tools.Bash{Sandbox: box.Command, Isolation: tools.Isolation{Tier: "process"}}, command)
		refused := eperm(sbx.Content) && !eperm(host.Content)
		out = append(out, labelled{name: name, refused: refused, source: "real", events: callEvents(command, agent.Observation{
			Content: sbx.Content, ExitCode: sbx.ExitCode, Sandbox: sbx.Tier, SandboxNote: sbx.SandboxNote})})
	}
	return out
}

// fenceCases are the fence's and git protection's refusals, which need Linux
// and the fence tier to happen for real, built from the payloads they write.
func fenceCases() []labelled {
	ev := func(typ agent.EventType, p map[string]any) agent.Event {
		p["call_id"] = "c1"
		raw, _ := json.Marshal(p)
		return agent.Event{Type: typ, Actor: agent.ActorSystem, Trust: agent.Trusted, Payload: raw}
	}
	exit := func(n int) *int { return &n }
	obs := func(content string, code int) agent.Observation {
		return agent.Observation{Content: content, ExitCode: exit(code), Sandbox: "fence"}
	}
	note := "\n\nNOTE: the sandbox denied this operation. Retrying the same command will fail identically."
	return []labelled{
		{"fork bomb at the process limit", true, "fence", callEvents(":(){ :|:& };:", obs("exit 254 · 2s\nbash: fork: retry: Resource temporarily unavailable", 254),
			ev(agent.EvFenceLimit, map[string]any{"pids_max": 40, "max_procs": 64, "oom": 0, "oom_kill": 0}))},
		{"killed at the memory limit", true, "fence", callEvents("python3 big.py", obs("exit 137 · 4s\nKilled", 137),
			ev(agent.EvFenceLimit, map[string]any{"oom": 3, "oom_kill": 1, "memory_max_mb": 512}))},
		{"group killed at the memory limit", true, "fence", callEvents("make -j64", obs("exit 2 · 9s\nmake: *** [all] Killed", 2),
			ev(agent.EvFenceLimit, map[string]any{"oom": 1, "oom_group_kill": 1, "memory_max_mb": 1024}))},
		{"refused at the session's limit", true, "fence", callEvents("xargs -P 200 -n1 true", obs("exit 1 · 1s\nxargs: cannot fork", 1),
			ev(agent.EvFenceLimit, map[string]any{"session_pids_max": 12}))},
		{"memory limit reached, nothing killed", false, "fence", callEvents("sort big.txt", obs("exit 0 · 3s\n", 0),
			ev(agent.EvFenceLimit, map[string]any{"oom": 1, "memory_max_mb": 512}))},
		{"seccomp refuses unshare", true, "fence", callEvents("unshare -r true", agent.Observation{Content: "exit 1 · 4ms\nunshare: unshare failed: Operation not permitted" + note,
			ExitCode: exit(1), Sandbox: "fence", SandboxNote: "denied"},
			ev(agent.EvFenceDenied, map[string]any{"source": "seccomp", "calls": map[string]int{"unshare": 1}}))},
		{"seccomp refuses unshare, landlock_only", true, "fence", callEvents("unshare -r true", agent.Observation{Content: "exit 1 · 4ms\nunshare: unshare failed: Operation not permitted" + note,
			ExitCode: exit(1), Sandbox: "fence", SandboxNote: "denied"})},
		{"seccomp refuses a unix socket", true, "fence", callEvents("python3 -c 'import socket; socket.socket(socket.AF_UNIX)'", agent.Observation{
			Content: "exit 1 · 30ms\nPermissionError: [Errno 1] Operation not permitted" + note, ExitCode: exit(1), Sandbox: "fence", SandboxNote: "denied"},
			ev(agent.EvFenceDenied, map[string]any{"source": "seccomp", "calls": map[string]int{"socket(unix)": 1}}))},
		{"landlock refuses a write", true, "fence", callEvents("touch /home/u/x", obs("exit 1 · 3ms\ntouch: cannot touch '/home/u/x': Permission denied", 1))},
		{"a planted git hook", true, "git", callEvents("git status", obs("sandbox: the command was not run: a repository made by an earlier command names a program git would run", 1),
			ev("sandbox.git_planted", map[string]any{"entries": []string{"sub/.git/hooks/pre-commit"}, "sandbox": "fence"}))},
		{"a linked git folder", true, "git", callEvents("git log", obs("sandbox: the command was not run: .git/config is a symbolic link", 1),
			ev("sandbox.git_linked", map[string]any{"paths": []string{".git/config"}, "refused": true}))},
		{"git walk bounded, not refused", false, "git", callEvents("ls", obs("exit 0 · 2ms\na", 0),
			ev("sandbox.git_walk_bounded", map[string]any{"bound": 20000, "refused": false}))},
		{"git walk bounded, refused", true, "git", callEvents("git status", obs("sandbox: the command was not run: the workspace has more than 20000 folders", 1),
			ev("sandbox.git_walk_bounded", map[string]any{"bound": 20000, "refused": true}))},
		{"egress refused (network-denied's)", false, "fence", callEvents("curl https://example.com", obs("exit 56 · 40ms\ncurl: (56) CONNECT tunnel failed, response 403", 56),
			ev(agent.EvEgressDecision, map[string]any{"kind": "connect", "host": "example.com", "port": 443, "decision": "deny", "rule": "default"}))},
	}
}
