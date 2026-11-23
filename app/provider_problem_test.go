package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/model"
)

// retiredServer answers every call as Google does for a retired model.
func retiredServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":404,"message":"Gemini 2.5 Pro is no longer available to new users.","status":"NOT_FOUND"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// In Studio, a retired default model's refusal is said in the chat with the
// provider's words and the next step naming a configured model to use; the
// next session names it before its first prompt.
func TestStudioSaysTheProvidersError(t *testing.T) {
	old := unavailableFile
	file := filepath.Join(t.TempDir(), "models-unavailable.json")
	unavailableFile = func() string { return file }
	t.Cleanup(func() { unavailableFile = old })
	gone := retiredServer(t)
	m := newScriptModel(t, say("ok"))
	acpModelsEnv(t, `{"model":{"default":"old","providers":{"old":`+stubProvider(gone.URL, "gemini-2.5-pro")+`,"good":`+stubProvider(m.URL, "m-1")+`}}}`)
	ws := t.TempDir()
	r := &studioRig{t: t, ws: ws, model: m}
	r.cl = newStudioClient(t, ws, true)
	r.cl.ok("initialize", map[string]any{"protocolVersion": 1}, nil)
	id := r.open()
	from := r.cl.mark()
	var res struct {
		StopReason string         `json:"stopReason"`
		Meta       map[string]any `json:"_meta"`
	}
	r.cl.ok("session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": "hi"}}}, &res)
	said := ""
	for _, u := range updates(r.cl.since(from)) {
		if c, ok := u["content"].(map[string]any); ok && u["sessionUpdate"] == "agent_message_chunk" {
			said += fmt.Sprint(c["text"])
		}
	}
	if res.StopReason != "refusal" || !strings.Contains(said, "no longer available to new users") || !strings.Contains(said, "Next:") || !strings.Contains(said, "Switch to good") {
		t.Fatalf("stop %s, said %q", res.StopReason, said)
	}
	pe, _ := res.Meta[acpMetaKey].(map[string]any)["providerError"].(map[string]any)
	if pe["kind"] != model.ProblemRetired || pe["provider"] != "old" {
		t.Fatalf("meta %v", res.Meta)
	}
	// The next session says so before its first prompt.
	var opened struct {
		Meta map[string]any `json:"_meta"`
	}
	r.cl.ok("session/new", map[string]any{"cwd": ws, "mcpServers": []any{}}, &opened)
	w, _ := opened.Meta[acpMetaKey].(map[string]any)["modelWarning"].(map[string]any)
	if msg, _ := w["message"].(string); !strings.Contains(msg, "gemini-2.5-pro") || !strings.Contains(msg, "Pick another: good") {
		t.Fatalf("warning %v", opened.Meta)
	}
}

// The next step names only configured models whose key is at hand, and a
// model that answered is no longer remembered as unavailable.
func TestExplainProviderAndRemembering(t *testing.T) {
	old := unavailableFile
	file := filepath.Join(t.TempDir(), "u.json")
	unavailableFile = func() string { return file }
	t.Cleanup(func() { unavailableFile = old })
	t.Setenv("NO_SUCH_KEY_FOR_TEST", "")
	cfg := config.Default()
	cfg.Model.Default = "a"
	cfg.Model.Providers = map[string]config.ProviderConfig{
		"a":     {Type: "gemini", Model: "gemini-2.5-pro", APIKey: "k"},
		"b":     {Type: "anthropic", Model: "claude-x", APIKey: "k"},
		"nokey": {Type: "openai", Model: "gpt-x", APIKeyEnv: "NO_SUCH_KEY_FOR_TEST"},
	}
	err := &model.StatusError{Status: 401, Body: `{"error":{"message":"API key not valid"}}`}
	pp := explainProvider(cfg, "a", err)
	if pp == nil || pp.Kind != model.ProblemKey || strings.Join(pp.Others, ",") != "b" || !strings.Contains(pp.Text(), "API key not valid") {
		t.Fatalf("%+v", pp)
	}
	if explainProvider(cfg, "a", fmt.Errorf("dial tcp: refused")) != nil {
		t.Fatal("a dial error explained as the provider's")
	}
	if defaultModelWarning(cfg) != "" {
		t.Fatal("warned before anything was remembered")
	}
	noteProviderProblem(pp) // a key problem is not remembered
	if defaultModelWarning(cfg) != "" {
		t.Fatal("a key problem was remembered")
	}
	noteProviderProblem(explainProvider(cfg, "a", &model.StatusError{Status: 404, Body: "model not found"}))
	if w := defaultModelWarning(cfg); !strings.Contains(w, "Pick another: b") {
		t.Fatalf("warning %q", w)
	}
	modelAnswered("a", "gemini-2.5-pro")
	if defaultModelWarning(cfg) != "" {
		t.Fatal("still remembered after it answered")
	}
}

// Doctor's model probe hands its failure on, so doctor can name the next step.
func TestDoctorProbeHandsOnTheFailure(t *testing.T) {
	gone := retiredServer(t)
	a, err := config.ProviderConfig{Type: "openai-compatible", BaseURL: gone.URL, Model: "gemini-2.5-pro"}.Adapter()
	if err != nil {
		t.Fatal(err)
	}
	var got error
	var out strings.Builder
	if doctorModelProbes(a, 5*time.Second, &out, func(err error) { got = err }) {
		t.Fatal("a retired model passed")
	}
	if p := model.ProblemOf(got); p == nil || p.Kind != model.ProblemRetired {
		t.Fatalf("handed on %v", got)
	}
}
