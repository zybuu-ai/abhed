package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// acpModelStub is a model endpoint that answers with its own text and counts
// its calls; with hold set, each call waits for release.
type acpModelStub struct {
	*httptest.Server
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// free lets held calls answer; a failing test frees them so Close can return.
func (st *acpModelStub) free() { st.once.Do(func() { close(st.release) }) }

func newACPModelStub(t *testing.T, answer string, hold bool) *acpModelStub {
	t.Helper()
	st := &acpModelStub{entered: make(chan struct{}, 1), release: make(chan struct{})}
	if !hold {
		st.free()
	}
	st.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		st.calls.Add(1)
		select {
		case st.entered <- struct{}{}:
		default:
		}
		<-st.release
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", answer)
	}))
	t.Cleanup(st.Close)
	t.Cleanup(st.free) // runs first
	return st
}

func stubProvider(url, model string, extra ...string) string {
	return `{"type":"openai-compatible","base_url":"` + url + `","model":"` + model + `","context_window":8192` +
		strings.Join(extra, "") + `}`
}

// acpModelsEnv isolates the process's configuration: userCfg is the person's
// own file, which is trusted; no managed file and no workspace trust.
func acpModelsEnv(t *testing.T, userCfg string) (captured func() []*abhed.Agent) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	t.Setenv(config.TrustEnv, "")
	old := managed.ConfigFile
	managed.ConfigFile = filepath.Join(t.TempDir(), "absent.json")
	t.Cleanup(func() { managed.ConfigFile = old })
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(userCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var made []*abhed.Agent
	oldAgent := newACPAgent
	t.Cleanup(func() { newACPAgent = oldAgent })
	newACPAgent = func(ctx context.Context, o abhed.Options) (acpAgent, error) {
		a, err := abhed.New(ctx, o)
		if err == nil {
			mu.Lock()
			made = append(made, a)
			mu.Unlock()
		}
		return a, err
	}
	return func() []*abhed.Agent {
		mu.Lock()
		defer mu.Unlock()
		return append([]*abhed.Agent(nil), made...)
	}
}

type acpModelOption struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Category     string `json:"category"`
	Type         string `json:"type"`
	CurrentValue string `json:"currentValue"`
	Options      []struct {
		Value       string `json:"value"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"options"`
}

type acpNewSession struct {
	SessionID     string           `json:"sessionId"`
	ConfigOptions []acpModelOption `json:"configOptions"`
	Models        struct {
		CurrentModelID  string `json:"currentModelId"`
		AvailableModels []struct {
			ModelID     string `json:"modelId"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"availableModels"`
	} `json:"models"`
}

func acpOpen(t *testing.T, cl *acpClient, ws string) acpNewSession {
	const id = 1
	t.Helper()
	cl.request(id, "initialize", map[string]any{"protocolVersion": 1})
	created := cl.request(id+1, "session/new", map[string]any{"cwd": ws})
	if created.Error != nil {
		t.Fatalf("session/new: %s", created.Error.Message)
	}
	var s acpNewSession
	if err := json.Unmarshal(created.Result, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// selectedModels is the options of the one model selector, as "name=description".
func selectedModels(t *testing.T, opts []acpModelOption) (current string, listed []string) {
	t.Helper()
	if len(opts) != 1 || opts[0].ID != "model" || opts[0].Category != "model" || opts[0].Type != "select" || opts[0].Name == "" {
		t.Fatalf("config options: %+v, want one select of category model", opts)
	}
	for _, o := range opts[0].Options {
		listed = append(listed, o.Value+"="+o.Name+"="+o.Description)
	}
	return opts[0].CurrentValue, listed
}

func switchedTo(a *abhed.Agent) []string {
	var out []string
	for _, ev := range a.Events() {
		if ev.Type == abhed.EvModelSwitched {
			var p agent.ModelSwitched
			_ = json.Unmarshal(ev.Payload, &p)
			out = append(out, p.Provider+":"+p.Model)
		}
	}
	return out
}

func TestACPListsAndSwitchesModels(t *testing.T) {
	a1, b1 := newACPModelStub(t, "from alpha", false), newACPModelStub(t, "from beta", false)
	agents := acpModelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+stubProvider(a1.URL, "m-a")+
		`,"beta":`+stubProvider(b1.URL, "m-b", `,"api_key_env":"ABHED_TEST_BETA_KEY"`)+`}}}`)
	t.Setenv("ABHED_TEST_BETA_KEY", "sk-beta-value")
	ws := t.TempDir()
	cl := newACPClient(t, nil)
	s := acpOpen(t, cl, ws)

	current, listed := selectedModels(t, s.ConfigOptions)
	want := []string{"alpha=alpha=m-a (openai-compatible)", "beta=beta=m-b (openai-compatible)"}
	if current != "alpha" || fmt.Sprint(listed) != fmt.Sprint(want) {
		t.Fatalf("session/new model selector: current %q, %v; want alpha, %v", current, listed, want)
	}
	if s.Models.CurrentModelID != "alpha" || len(s.Models.AvailableModels) != 2 ||
		s.Models.AvailableModels[1].ModelID != "beta" || s.Models.AvailableModels[1].Description != "m-b (openai-compatible)" {
		t.Fatalf("session/new models: %+v", s.Models)
	}
	raw, _ := json.Marshal(s)
	for _, leak := range []string{a1.URL, b1.URL, "127.0.0.1", "ABHED_TEST_BETA_KEY", "sk-beta-value"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the model list carries %q: %s", leak, raw)
		}
	}

	res := cl.request(3, "session/set_config_option", map[string]any{"sessionId": s.SessionID, "configId": "model", "value": "beta"})
	if res.Error != nil {
		t.Fatalf("set_config_option: %s", res.Error.Message)
	}
	var set struct {
		ConfigOptions []acpModelOption `json:"configOptions"`
	}
	_ = json.Unmarshal(res.Result, &set)
	if cur, _ := selectedModels(t, set.ConfigOptions); cur != "beta" {
		t.Fatalf("the reply does not report beta as current: %s", res.Result)
	}
	_, updates := cl.prompt(4, map[string]any{"sessionId": s.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "hi"}}})
	// The reply carried the options; the notification is for the agent's own changes.
	for _, u := range updates {
		if u["sessionUpdate"] == "config_option_update" {
			t.Fatalf("a config_option_update followed the editor's own set_config_option: %v", u)
		}
	}
	if !strings.Contains(fmt.Sprint(updates), "from beta") || a1.calls.Load() != 0 {
		t.Fatalf("beta did not answer after the switch (alpha calls %d): %v", a1.calls.Load(), updates)
	}

	// The unstable session/set_model is answered too.
	res = cl.request(5, "session/set_model", map[string]any{"sessionId": s.SessionID, "modelId": "alpha"})
	if res.Error != nil || !strings.Contains(string(res.Result), `"currentModelId":"alpha"`) {
		t.Fatalf("set_model: %s %+v", res.Result, res.Error)
	}
	made := agents()
	if len(made) != 1 {
		t.Fatalf("agents: %d", len(made))
	}
	if got := switchedTo(made[0]); fmt.Sprint(got) != "[beta:m-b alpha:m-a]" {
		t.Fatalf("model.switched in the record: %v", got)
	}
}

func TestACPRefusesAnUnknownModel(t *testing.T) {
	a1 := newACPModelStub(t, "a", false)
	agents := acpModelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+stubProvider(a1.URL, "m-a")+`}}}`)
	cl := newACPClient(t, nil)
	s := acpOpen(t, cl, t.TempDir())
	id := 3
	for _, params := range []map[string]any{
		{"configId": "model", "value": "nope"},
		{"configId": "model", "value": "ollama"},  // a built-in type nobody configured
		{"configId": "model", "value": a1.URL},    // an endpoint is never taken
		{"configId": "model", "value": true},      // not a name
		{"configId": "thinking", "value": "high"}, // not an option this agent has
	} {
		params["sessionId"] = s.SessionID
		res := cl.request(id, "session/set_config_option", params)
		id++
		if res.Error == nil || res.Error.Code != -32602 {
			t.Errorf("set_config_option %v: %s %+v, want invalid params", params, res.Result, res.Error)
		}
	}
	if res := cl.request(id, "session/set_model", map[string]any{"sessionId": s.SessionID, "modelId": "nope"}); res.Error == nil ||
		!strings.Contains(res.Error.Message, `"nope"`) {
		t.Errorf("set_model nope: %+v", res.Error)
	}
	if got := switchedTo(agents()[0]); len(got) != 0 {
		t.Fatalf("a refused switch was recorded: %v", got)
	}
}

func TestACPRefusesASwitchDuringAPrompt(t *testing.T) {
	a1, b1 := newACPModelStub(t, "a", true), newACPModelStub(t, "b", false)
	agents := acpModelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+stubProvider(a1.URL, "m-a")+
		`,"beta":`+stubProvider(b1.URL, "m-b")+`}}}`)
	cl := newACPClient(t, nil)
	s := acpOpen(t, cl, t.TempDir())
	raw, _ := json.Marshal(map[string]any{"sessionId": s.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "hi"}}})
	cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage("3"), Method: "session/prompt", Params: raw})
	select {
	case <-a1.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the prompt never reached the model")
	}
	res := cl.request(4, "session/set_config_option", map[string]any{"sessionId": s.SessionID, "configId": "model", "value": "beta"})
	if res.Error == nil || !strings.Contains(res.Error.Message, "prompt is running") {
		t.Fatalf("switch during a prompt: %s %+v", res.Result, res.Error)
	}
	a1.free()
	deadline := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case m := <-cl.lines:
			done = m.Method == "" && string(m.ID) == "3"
		case <-deadline:
			t.Fatal("the prompt never ended")
		}
	}
	if got := switchedTo(agents()[0]); len(got) != 0 {
		t.Fatalf("a refused switch was recorded: %v", got)
	}
	if res := cl.request(5, "session/set_config_option", map[string]any{"sessionId": s.SessionID, "configId": "model", "value": "beta"}); res.Error != nil {
		t.Fatalf("switch after the prompt: %s", res.Error.Message)
	}
}

// A workspace file nobody trusted adds no model to the list, and its name cannot be switched to.
func TestACPUntrustedWorkspaceModelIsNotOffered(t *testing.T) {
	a1, evil := newACPModelStub(t, "a", false), newACPModelStub(t, "evil", false)
	acpModelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+stubProvider(a1.URL, "m-a")+`}}}`)
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	wsCfg := `{"model":{"default":"evil","providers":{"evil":` + stubProvider(evil.URL, "m-evil") + `}}}`
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(wsCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cl := newACPClient(t, nil)
	s := acpOpen(t, cl, ws)
	current, listed := selectedModels(t, s.ConfigOptions)
	if current != "alpha" || len(listed) != 1 || strings.Contains(fmt.Sprint(listed), "evil") {
		t.Fatalf("untrusted workspace: current %q, listed %v", current, listed)
	}
	res := cl.request(3, "session/set_config_option", map[string]any{"sessionId": s.SessionID, "configId": "model", "value": "evil"})
	if res.Error == nil {
		t.Fatalf("switched to the untrusted workspace's model: %s", res.Result)
	}
	cl.prompt(4, map[string]any{"sessionId": s.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "hi"}}})
	if evil.calls.Load() != 0 {
		t.Fatal("the untrusted workspace's endpoint was called")
	}
}

// A provider whose key variable is unset is listed; choosing it is refused
// with the variable named.
func TestACPModelWithoutItsKey(t *testing.T) {
	a1, b1 := newACPModelStub(t, "a", false), newACPModelStub(t, "b", false)
	agents := acpModelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+stubProvider(a1.URL, "m-a")+
		`,"beta":`+stubProvider(b1.URL, "m-b", `,"api_key_env":"ABHED_TEST_BETA_KEY"`)+`}}}`)
	t.Setenv("ABHED_TEST_BETA_KEY", "")
	cl := newACPClient(t, nil)
	s := acpOpen(t, cl, t.TempDir())
	if _, listed := selectedModels(t, s.ConfigOptions); len(listed) != 2 {
		t.Fatalf("a model without its key is not listed: %v", listed)
	}
	res := cl.request(3, "session/set_config_option", map[string]any{"sessionId": s.SessionID, "configId": "model", "value": "beta"})
	if res.Error == nil || !strings.Contains(res.Error.Message, "ABHED_TEST_BETA_KEY") || strings.Contains(res.Error.Message, b1.URL) {
		t.Fatalf("switch without the key: %+v", res.Error)
	}
	if got := switchedTo(agents()[0]); len(got) != 0 {
		t.Fatalf("a refused switch was recorded: %v", got)
	}
}

// switchingACPAgent holds a prompt until released and switches whatever it is
// told to, so the adapter's own refusal during a prompt is what is tested.
type switchingACPAgent struct {
	entered, release chan struct{}
	mu               sync.Mutex
	switched         []string
}

func (a *switchingACPAgent) Run(ctx context.Context, _ string) (string, error) {
	a.entered <- struct{}{}
	select {
	case <-a.release:
	case <-ctx.Done():
	}
	return "", nil
}
func (a *switchingACPAgent) Steer(string)                {}
func (a *switchingACPAgent) Flush(context.Context) error { return nil }
func (a *switchingACPAgent) Close()                      {}
func (a *switchingACPAgent) Models() []abhed.Model {
	return []abhed.Model{{Name: "alpha", Model: "m-a", Type: "ollama", Current: true}, {Name: "beta", Model: "m-b", Type: "ollama"}}
}
func (a *switchingACPAgent) SwitchModelNamed(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.switched = append(a.switched, name)
	return nil
}

func TestACPAdapterRefusesASwitchDuringAPrompt(t *testing.T) {
	fake := &switchingACPAgent{entered: make(chan struct{}, 1), release: make(chan struct{})}
	oldAgent := newACPAgent
	t.Cleanup(func() { newACPAgent = oldAgent })
	newACPAgent = func(context.Context, abhed.Options) (acpAgent, error) { return fake, nil }
	cl := newACPClient(t, nil)
	s := acpOpen(t, cl, "/ws")
	raw, _ := json.Marshal(map[string]any{"sessionId": s.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "hi"}}})
	cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage("3"), Method: "session/prompt", Params: raw})
	select {
	case <-fake.entered:
	case <-time.After(10 * time.Second):
		close(fake.release)
		t.Fatal("the prompt never reached the agent")
	}
	res := cl.request(4, "session/set_model", map[string]any{"sessionId": s.SessionID, "modelId": "beta"})
	close(fake.release)
	if res.Error == nil || !strings.Contains(res.Error.Message, "prompt is running") {
		t.Fatalf("switch during a prompt: %s %+v", res.Result, res.Error)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.switched) != 0 {
		t.Fatalf("the agent was switched during a prompt: %v", fake.switched)
	}
}
