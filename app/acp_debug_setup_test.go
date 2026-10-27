package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// setupRig is a Studio session whose commands run on the process tier, with
// the engine probe and the consent file its own.
func setupRig(t *testing.T, engine, brew, machine string) (*studioRig, string, *[]string) {
	t.Helper()
	asked := fakeDebugger(t, 0)
	debugTier = func(tools.Bash) string { return "process" }
	oldProbe, oldFile := debugEngineProbe, debugConsentFile
	debugEngineProbe = func(context.Context) (string, string, string) { return engine, brew, machine }
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
	r, id, asked := setupRig(t, "", "/opt/homebrew/bin/brew", "")
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
		!d.Setup.ProcessTier.Available || d.Setup.ProcessTier.Consented || !strings.Contains(d.Setup.ProcessTier.Consent, "localhost ports") {
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
	r, id, asked := setupRig(t, "", "", "")
	var reasons []string
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
		}
		_ = json.Unmarshal(params, &p)
		reasons = append(reasons, fmt.Sprint(p.ToolCall.Meta[acpMetaKey]["reason"], " step=", p.ToolCall.Meta[acpMetaKey]["step"]))
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": answer + p.ToolCall.ToolCallID}}
	})
	launch := map[string]any{"sessionId": id, "language": "python", "program": "app.py", "processTier": true}
	r.cl.refused(errPolicy, "_abhed/debug/launch", launch)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "localhost ports") || !strings.Contains(reasons[0], "step=workspace") || len(*asked) != 0 || debugConsented(r.ws) {
		t.Fatalf("declined: asked %v, ran %v", reasons, *asked)
	}
	answer = "once:"
	var res struct {
		DebugID string `json:"debugId"`
	}
	r.cl.ok("_abhed/debug/launch", launch, &res)
	if len(reasons) != 2 || len(*asked) != 1 || !debugConsented(r.ws) {
		t.Fatalf("allowed: asked %v, ran %v", reasons, *asked)
	}
	r.cl.ok("_abhed/debug/stop", map[string]any{"debugId": res.DebugID}, nil)
	setups, _ := r.recorded(id, agent.EvDebugSetup)
	launched, _ := r.recorded(id, agent.EvDebugLaunched)
	if len(setups) != 1 || setups[0]["action"] != "process_tier_consent" || len(launched) != 1 || launched[0]["loopback"] != true {
		t.Fatalf("record: %v %v", setups, launched)
	}
	// Agreed once for the workspace: not asked again.
	r.cl.ok("_abhed/debug/launch", launch, &res)
	if len(reasons) != 2 {
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
	r, id, _ := setupRig(t, "", "", "")
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
	if setups, _ := r.recorded(id, agent.EvDebugSetup); len(setups) != 1 || setups[0]["action"] != "use_container" {
		t.Fatalf("record %v", setups)
	}
}
