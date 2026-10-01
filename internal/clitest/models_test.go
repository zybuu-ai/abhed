//go:build unix

package clitest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Two configured providers on the one stub, told apart by their model ids.
const twoModels = `{"sandbox":{"min_tier":"none"},"model":{"default":"a","providers":{` +
	`"a":{"type":"openai-compatible","base_url":"{{MODEL_URL}}","model":"model-a","context_window":32768},` +
	`"b":{"type":"openai-compatible","base_url":"{{MODEL_URL}}","model":"model-b","context_window":65536}}}}`

// A model that refuses access (403) moves the run to the fallback, which is
// recorded as model.fallback, and the answer comes from the fallback.
func TestFallbackOnAccessRefused(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{UserConfig: twoModels, Args: []string{"-p", "hi", "-fallback-model", "b", "-output-format", "stream-json"},
		Script: "error 403\n\ntext \"from b\""})
	if code := h.Wait(time.Second); code != 0 {
		t.Fatalf("exit %d:\n%s", code, h.Output())
	}
	var fb *agent.ModelFallback
	for _, e := range ParseEvents(h.Stdout()) {
		if e.Type == agent.EvModelFallback {
			fb = &agent.ModelFallback{}
			_ = json.Unmarshal(e.Payload, fb)
		}
	}
	if fb == nil || fb.From != "a" || fb.To != "b" || !strings.Contains(fb.Reason, "403") {
		t.Fatalf("fallback record %+v:\n%s", fb, h.Stdout())
	}
	reqs := h.Requests()
	if len(reqs) != 2 || !strings.Contains(string(reqs[1].Body), `"model-b"`) {
		t.Fatalf("the second request did not go to model-b")
	}
	if resultOf(t, h.Stdout())["result"] != "from b" {
		t.Fatalf("result:\n%s", h.Stdout())
	}
}

// A fallback that is not a configured provider is left out with a warning,
// and a managed model.default is never left.
func TestFallbackOnlyToOfferedModels(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{UserConfig: twoModels, Args: []string{"-p", "hi", "-fallback-model", "nope"}, Script: `error 403`})
	if code := h.Wait(time.Second); code != 1 || !strings.Contains(h.Stderr(), `fallback "nope" is not a configured provider`) {
		t.Fatalf("exit %d:\n%s", code, h.Stderr())
	}
	m := piped(t, Opts{UserConfig: twoModels, Managed: `{"model":{"default":"a"}}`, Args: []string{"-p", "hi", "-fallback-model", "b"}, Script: `error 403`})
	if code := m.Wait(time.Second); code != 1 || !strings.Contains(m.Stderr(), "no fallback is used") || len(m.Requests()) != 1 {
		t.Fatalf("exit %d, %d requests:\n%s", code, len(m.Requests()), m.Stderr())
	}
}

// -model is refused under a managed model.default, naming the run's own
// managed file by its absolute path; a -fallback-model is ignored when the
// managed file names the fallbacks.
func TestManagedModelBindsTheFlags(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{UserConfig: twoModels, Managed: `{"model":{"default":"a"}}`, Args: []string{"-p", "hi", "-model", "b"}})
	want := filepath.Join(h.root, "etc", "abhed", "config.json")
	if code := h.Wait(time.Second); code != 2 || !strings.Contains(h.Stderr(), "refused") || !strings.Contains(h.Stderr(), want) {
		t.Fatalf("exit %d, want the refusal naming %s:\n%s", code, want, h.Stderr())
	}
	m := piped(t, Opts{UserConfig: twoModels, Managed: `{"model":{"default":"a","fallback":[]}}`,
		Args: []string{"-p", "hi", "-fallback-model", "b"}, Script: `error 403`})
	if code := m.Wait(time.Second); code != 1 || !strings.Contains(m.Stderr(), "-fallback-model is ignored") || len(m.Requests()) != 1 {
		t.Fatalf("exit %d, %d requests:\n%s", code, len(m.Requests()), m.Stderr())
	}
}

// /model lists the configured models with what they are, and switches by
// name; /status and /usage show the session.
func TestModelStatusUsageOnPty(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{UserConfig: twoModels, Cols: 120, Rows: 40, Script: "text \"one\"\nusage in=1000 cached=800 out=5\n\ntext \"two\""})
	h.WaitText("Type a task")
	h.Settle()
	h.Type("/model\r")
	h.WaitText("model-b · 64k context · local")
	time.Sleep(400 * time.Millisecond) // a dialog takes no key in its first 300 ms
	h.Key(Esc)                         // the list is a pick; Esc leaves the model as it is
	h.WaitText("unchanged; /model <name> switches")
	h.Settle()
	h.Type("hi\r")
	h.WaitText("● one")
	h.Settle()
	h.Type("/model b\r")
	h.WaitText("switched to model-b")
	closePanel(h)
	h.Settle()
	h.Type("/usage\r")
	h.WaitText("prefill saving")
	closePanel(h)
	h.Settle()
	h.Type("/status\r")
	h.WaitText("turn limit")
	s := h.WaitText("record")
	if !s.Contains("model-b (b)") || !s.Contains("local record, chained") {
		t.Fatalf("status:\n%s", s.Text())
	}
	closePanel(h)
	h.Settle()
	h.Type("/effort\r")
	h.WaitText("effort default")
	closePanel(h)
	h.Settle()
	h.Type("again\r")
	h.WaitText("● two")
	h.Exit(0)
	if !strings.Contains(string(h.Requests()[1].Body), `"model-b"`) {
		t.Fatal("the switch did not reach the model")
	}
}

// /config shows where each setting comes from; set writes the person's own
// file, tightening at once, and a widening change is not made without a
// confirmation. A managed setting is refused.
func TestConfigCommand(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{Cols: 160, Rows: 40, Managed: `{"limits":{"max_turns":50}}`})
	h.WaitText("Type a task")
	h.Settle()
	h.Type("/config\r")
	h.WaitText("limits.max_turns")
	if s := h.Screen(); !strings.Contains(s.Text(), "managed") {
		t.Fatalf("no source shown:\n%s", s.Text())
	}
	closePanel(h)
	h.Settle()
	h.Type("/config set permissions.mode plan\r")
	h.WaitText("permissions.mode set in")
	closePanel(h)
	h.Settle()
	h.Type("/config set sandbox.allow_network true\r")
	h.WaitText("in your own configuration?")
	time.Sleep(400 * time.Millisecond) // a number counts only with quiet around it
	h.Type("2")
	h.WaitText("sandbox.allow_network not changed")
	closePanel(h)
	h.Settle()
	h.Type("/config set limits.max_turns 5\r")
	h.WaitText("set by the managed configuration")
	closePanel(h)
	h.Exit(0)
	data, _ := os.ReadFile(filepath.Join(h.Home(), ".abhed", "config.json"))
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	perms, _ := cfg["permissions"].(map[string]any)
	if perms["mode"] != "plan" || cfg["model"] == nil || strings.Contains(string(data), "allow_network") {
		t.Fatalf("config:\n%s", data)
	}
}
