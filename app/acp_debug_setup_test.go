package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/debugrelay"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// setupRig is a Studio session whose commands run on the process tier, with
// the engine probe and the consent file its own.
func setupRig(t *testing.T, brew string) (*studioRig, string, *[]string) {
	t.Helper()
	asked := fakeDebugger(t, 0)
	debugTier = func(tools.Bash) string { return "process" }
	oldProbe, oldFile := debugEngineProbe, debugConsentFile
	debugEngineProbe = func(context.Context) (string, string, string) { return "", brew, "" }
	file := filepath.Join(t.TempDir(), "debug-process-tier.json")
	debugConsentFile = func() string { return file }
	t.Cleanup(func() { debugEngineProbe, debugConsentFile = oldProbe, oldFile })
	r := newStudioRig(t, `,"monitor":{"enabled":false}`)
	id := r.open()
	r.write("app.py", "print('hi')\n")
	return r, id, asked
}

// A tier that cannot debug says what the container tier needs here, as
// steps with their commands, and that the process tier is an option.
func TestDebugContainerSetupProblem(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the macOS process tier's setup")
	}
	r, id, asked := setupRig(t, "/opt/homebrew/bin/brew")
	m := r.cl.call("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py"})
	if m.Error == nil || len(*asked) != 0 {
		t.Fatalf("launch %+v, asked %v", m.Error, *asked)
	}
	data, _ := json.Marshal(m.Error.Data)
	var d struct {
		Problem struct {
			Kind  string   `json:"kind"`
			Steps []string `json:"steps"`
		} `json:"problem"`
		Setup debugSetup `json:"setup"`
	}
	if json.Unmarshal(data, &d) != nil || d.Problem.Kind != "container-setup" {
		t.Fatalf("problem %s", data)
	}
	var ids []string
	for _, s := range d.Setup.Steps {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "install-podman,machine-init,machine-start,use-container-tier" || d.Setup.Steps[0].Command != "brew install podman" ||
		!d.Setup.ProcessTier.Available || d.Setup.ProcessTier.Consented || !strings.Contains(d.Setup.ProcessTier.Consent, "five ports Abhed picks") {
		t.Fatalf("setup %+v", d.Setup)
	}
	if !strings.Contains(strings.Join(d.Problem.Steps, "\n"), "processTier: true") {
		t.Fatalf("steps %v", d.Problem.Steps)
	}
	// A stopped machine needs only starting; the status says the same.
	debugEngineProbe = func(context.Context) (string, string, string) { return "/opt/homebrew/bin/podman", "", "stopped" }
	var st debugSetup
	r.cl.ok("_abhed/debug/status-tier", map[string]any{"sessionId": id}, &st)
	if st.Ready || len(st.Steps) != 2 || st.Steps[0].Command != "/opt/homebrew/bin/podman machine start" || st.Steps[1].ID != "use-container-tier" {
		t.Fatalf("status %+v", st)
	}
}

// On the person's choice, the macOS process tier debugs after their
// agreement for the workspace, asked once with the plain consequence, kept
// and recorded; a refusal keeps nothing; forgetting asks again.
func TestDebugProcessTierConsent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the macOS process tier's consent")
	}
	r, id, asked := setupRig(t, "")
	var reasons []string
	var options [][]string
	answer := "reject:"
	r.cl.answering(func(method string, params json.RawMessage) any {
		if method != "session/request_permission" {
			return map[string]any{}
		}
		var p struct {
			ToolCall struct {
				ToolCallID string                    `json:"toolCallId"`
				Meta       map[string]map[string]any `json:"_meta"`
			} `json:"toolCall"`
			Options []struct {
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"options"`
		}
		_ = json.Unmarshal(params, &p)
		reasons = append(reasons, fmt.Sprint(p.ToolCall.Meta[acpMetaKey]["reason"], " step=", p.ToolCall.Meta[acpMetaKey]["step"]))
		var opts []string
		for _, o := range p.Options {
			opts = append(opts, o.Kind+"="+o.Name)
		}
		options = append(options, opts)
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": answer + p.ToolCall.ToolCallID}}
	})
	launch := map[string]any{"sessionId": id, "language": "python", "program": "app.py", "processTier": true}
	r.cl.refused(errPolicy, "_abhed/debug/launch", launch)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "five ports Abhed picks") || !strings.Contains(reasons[0], "step=workspace") || len(*asked) != 0 || debugConsented(r.ws) {
		t.Fatalf("declined: asked %v, ran %v", reasons, *asked)
	}
	if strings.Join(options[0], ",") != "allow_once=Allow once,allow_always=Allow for this workspace,reject_once=Deny" {
		t.Fatalf("options %v", options[0])
	}
	// Allow once: this session starts, nothing is kept, the next is asked again.
	answer = "once:"
	var res struct {
		DebugID string `json:"debugId"`
	}
	r.cl.ok("_abhed/debug/launch", launch, &res)
	if len(reasons) != 2 || len(*asked) != 1 || debugConsented(r.ws) {
		t.Fatalf("allowed once: asked %v, ran %v, kept %v", reasons, *asked, debugConsented(r.ws))
	}
	r.cl.ok("_abhed/debug/stop", map[string]any{"debugId": res.DebugID}, nil)
	if setups, _ := r.recorded(id, agent.EvDebugSetup); len(setups) != 0 {
		t.Fatalf("allow once was recorded as the workspace's choice: %v", setups)
	}
	// Allow for this workspace: kept and recorded.
	answer = "always:"
	r.cl.ok("_abhed/debug/launch", launch, &res)
	if len(reasons) != 3 || len(*asked) != 2 || !debugConsented(r.ws) {
		t.Fatalf("allowed for the workspace: asked %v, ran %v", reasons, *asked)
	}
	r.cl.ok("_abhed/debug/stop", map[string]any{"debugId": res.DebugID}, nil)
	setups, _ := r.recorded(id, agent.EvDebugSetup)
	launched, _ := r.recorded(id, agent.EvDebugLaunched)
	if len(setups) != 1 || setups[0]["action"] != "process_tier_consent" || len(launched) != 2 || launched[1]["loopback"] != true {
		t.Fatalf("record: %v %v", setups, launched)
	}
	// Agreed for the workspace: not asked again.
	r.cl.ok("_abhed/debug/launch", launch, &res)
	if len(reasons) != 3 {
		t.Fatalf("asked again: %v", reasons)
	}
	r.cl.ok("_abhed/debug/stop", map[string]any{"debugId": res.DebugID}, nil)
	// Without processTier the setup problem stands, consent or not.
	if m := r.cl.call("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py"}); m.Error == nil {
		t.Fatal("launched on the process tier without being asked to")
	}
	r.cl.ok("_abhed/debug/forget-consent", map[string]any{"sessionId": id}, nil)
	if debugConsented(r.ws) {
		t.Fatal("the choice was not forgotten")
	}
}

// Use the container tier: sandbox.min_tier in the workspace's own file, the
// rest of it kept, recorded.
func TestDebugUseContainer(t *testing.T) {
	r, id, _ := setupRig(t, "")
	r.write(".abhed/config.json", `{"sandbox":{"image":"golang:1.26"},"model":{"default":"x"}}`)
	var res struct {
		Path string `json:"path"`
	}
	r.cl.ok("_abhed/debug/use-container", map[string]any{"sessionId": id}, &res)
	b, err := os.ReadFile(filepath.Join(r.ws, ".abhed", "config.json"))
	var cfg map[string]map[string]any
	if err != nil || json.Unmarshal(b, &cfg) != nil || cfg["sandbox"]["min_tier"] != "container" || cfg["sandbox"]["image"] != "golang:1.26" || cfg["model"]["default"] != "x" {
		t.Fatalf("file %s %v", b, err)
	}
	// A stronger tier is never lowered.
	r.write(".abhed/config.json", `{"sandbox":{"min_tier":"vm"}}`)
	var again struct {
		Already bool `json:"already"`
	}
	r.cl.ok("_abhed/debug/use-container", map[string]any{"sessionId": id}, &again)
	if b, _ := os.ReadFile(filepath.Join(r.ws, ".abhed", "config.json")); !again.Already || !strings.Contains(string(b), `"min_tier":"vm"`) {
		t.Fatalf("a vm workspace became %s (already %v)", b, again.Already)
	}
	if setups, _ := r.recorded(id, agent.EvDebugSetup); len(setups) != 1 || setups[0]["action"] != "use_container" {
		t.Fatalf("record %v", setups)
	}
}

// Where the process tier may debug on agreement, on any system.
func TestDebugProcessTierPossibleOn(t *testing.T) {
	closed, open, allow := tools.Isolation{}, tools.Isolation{Network: true}, tools.Isolation{Allowlist: true}
	for _, c := range []struct {
		goos, tier string
		iso        tools.Isolation
		ok         bool
	}{
		{"darwin", "process", closed, true}, {"darwin", "process", open, true},
		// Under the allowlist only the adapter's own ports are opened (Python;
		// Go is refused at launch).
		{"darwin", "process", allow, true},
		{"linux", "process", open, true}, {"linux", "process", closed, false}, {"linux", "process", allow, false},
		{"darwin", "container", closed, false}, {"darwin", "none", closed, false}, {"linux", "fence", open, false},
		{"windows", "process", open, false},
	} {
		if got := debugProcessTierPossibleOn(c.goos, c.tier, c.iso); got != c.ok {
			t.Errorf("%s %s %+v: %v", c.goos, c.tier, c.iso, got)
		}
	}
	if !strings.Contains(debugLoopbackWhyOn("linux"), "the network") || strings.Contains(debugLoopbackWhyOn("linux"), "nothing else on the network") ||
		!strings.Contains(debugLoopbackWhyOn("darwin"), "Nothing else on the network") {
		t.Fatal("the agreement's text does not fit its system")
	}
}

// An unreadable consent file counts as no agreement and is kept aside, not
// overwritten.
func TestDebugConsentFileUnreadable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "debug-process-tier.json")
	old := debugConsentFile
	debugConsentFile = func() string { return file }
	t.Cleanup(func() { debugConsentFile = old })
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if debugConsented("/w") {
		t.Fatal("an unreadable file gave an agreement")
	}
	if err := setDebugConsent("/w", true); err != nil {
		t.Fatal(err)
	}
	aside, _ := filepath.Glob(file + ".unreadable-*")
	if len(aside) != 1 || !debugConsented("/w") {
		t.Fatalf("aside %v", aside)
	}
	if b, _ := os.ReadFile(aside[0]); string(b) != "{not json" {
		t.Fatalf("kept %q", b)
	}
}

// Under the allowlist on macOS, Python debugs on the process tier with its
// adapter's ports pinned; Go is refused with steps.
func TestDebugProcessTierUnderAllowlist(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the macOS process tier")
	}
	r, id, asked := setupRig(t, "")
	s := r.cl.conn.session(id)
	tool, _ := s.parts.Loop.Tools.Get("bash")
	b := tool.(tools.Bash)
	b.Isolation.Allowlist = true
	s.parts.Loop.Tools.Add(b)
	if err := setDebugConsent(r.ws, true); err != nil {
		t.Fatal(err)
	}
	r.write("main.go", "package main\nfunc main() {}\n")
	old := lspProgramFinder
	lspProgramFinder = func(*tools.Registry) func(string) bool { return func(string) bool { return true } }
	t.Cleanup(func() { lspProgramFinder = old })
	// Go runs too: every loopback port but Abhed's reserved range.
	var res struct {
		DebugID string `json:"debugId"`
	}
	r.cl.ok("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "go", "program": ".", "processTier": true}, &res)
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "dlv dap") {
		t.Fatalf("go ran %v", *asked)
	}
	r.cl.ok("_abhed/debug/stop", map[string]any{"debugId": res.DebugID}, nil)
	r.cl.ok("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py", "processTier": true}, &res)
	if len(*asked) != 2 || !strings.Contains((*asked)[1], "ABHED_DAP_PORTS=") || strings.Count((*asked)[1], ",") < debugrelay.PinnedPorts-1 ||
		strings.Contains((*asked)[1], "exec python3 -m debugpy.adapter") {
		t.Fatalf("python ran %v", *asked)
	}
	r.cl.ok("_abhed/debug/stop", map[string]any{"debugId": res.DebugID}, nil)
}

// A launch the sandbox refuses to open loopback to is a problem with steps,
// and "Allow for this workspace" keeps nothing for a session that never
// started.
func TestDebugConsentKeptOnlyWhenStarted(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the macOS process tier")
	}
	r, id, _ := setupRig(t, "")
	debugCommand = func(_ tools.Bash, ctx context.Context, cwd, command string) *exec.Cmd {
		return &exec.Cmd{Err: sandbox.ErrLoopbackUnderEgress}
	}
	r.cl.answering(func(method string, params json.RawMessage) any {
		var p struct {
			ToolCall struct {
				ToolCallID string `json:"toolCallId"`
			} `json:"toolCall"`
		}
		_ = json.Unmarshal(params, &p)
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "always:" + p.ToolCall.ToolCallID}}
	})
	m := r.cl.call("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py", "processTier": true})
	if m.Error == nil || !strings.Contains(fmt.Sprint(m.Error.Data), "tier-unsupported") || !strings.Contains(m.Error.Message, "egress proxy") {
		t.Fatalf("launch %+v", m.Error)
	}
	if debugConsented(r.ws) {
		t.Fatal("a consent was kept for a session that never started")
	}
	if setups, _ := r.recorded(id, agent.EvDebugSetup); len(setups) != 0 {
		t.Fatalf("recorded %v", setups)
	}
}

// Delve missing where commands run: the install is offered, as a structured
// install for a one-click button, before anything is judged.
func TestDebugOffersDelveBeforeJudging(t *testing.T) {
	r, id, asked := setupRig(t, "")
	debugTier = func(tools.Bash) string { return "container" }
	old := lspProgramFinder
	lspProgramFinder = func(*tools.Registry) func(string) bool { return func(p string) bool { return p != "dlv" } }
	t.Cleanup(func() { lspProgramFinder = old })
	r.write("main.go", "package main\nfunc main() {}\n")
	m := r.cl.call("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "go", "program": "."})
	if m.Error == nil || len(*asked) != 0 {
		t.Fatalf("launch %+v ran %v", m.Error, *asked)
	}
	data, _ := json.Marshal(m.Error.Data)
	if !strings.Contains(string(data), `"install":{"debugger":true,"language":"go"}`) || !strings.Contains(string(data), "adapter-missing") ||
		strings.Contains(string(data), "_abhed/") {
		t.Fatalf("problem %s", data)
	}
	if reqs, _ := r.recorded(id, agent.EvActionRequested); len(reqs) != 0 {
		t.Fatalf("judged: %v", reqs)
	}
}
