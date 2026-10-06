package abhed_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/secrets"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// fetchingModel calls web_fetch on url once, then ends. It keeps the system
// prompt of the first request.
func fetchingModel(t *testing.T, url string, system *atomic.Value) string {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"url": url})
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if n.Add(1) == 1 {
			if system != nil {
				system.Store(string(body))
			}
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"f1","type":"function","function":{"name":"web_fetch","arguments":`+
				strconv.Quote(string(args))+`}}]}}]}`)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// configDir writes a trusted, empty configuration, and a managed one holding
// webFetch: only the managed configuration turns web fetch on.
func configDir(t *testing.T, webFetch string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".abhed", "config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `{}`
	if webFetch != "" {
		body = `{"web_fetch":` + webFetch + `}`
	}
	managedFile(t, body)
	return dir
}

// managedFile points the managed configuration at a file holding body.
func managedFile(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
}

// Neither a trusted ConfigDir nor the person's own file turns web search on
// for an embedded agent, or moves where a managed search sends its queries.
func TestEmbeddedConfigCannotEnableWebSearch(t *testing.T) {
	for _, m := range []string{`{}`, `{"web_search":{"enabled":true,"provider":"searxng","base_url":"http://127.0.0.1:9/managed"}}`} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		on := `{"web_search":{"enabled":true,"provider":"searxng","base_url":"http://127.0.0.1:9/sink"}}`
		for _, d := range []string{filepath.Join(home, ".abhed")} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(on), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".abhed"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".abhed", "config.json"), []byte(on), 0o600); err != nil {
			t.Fatal(err)
		}
		managedFile(t, m)
		var system atomic.Value
		a, err := abhed.New(context.Background(), abhed.Options{
			Workspace: dir, ConfigDir: dir, WorkspaceTrust: config.TrustGranted, ConfiguredTools: true,
			Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: fetchingModel(t, "https://docs.example.invalid/", &system),
				Model: "m", ContextWindow: 8192},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Run(context.Background(), "search"); err != nil {
			t.Fatal(err)
		}
		a.Close()
		body, _ := system.Load().(string)
		offered := strings.Contains(body, `"name":"web_search"`)
		if want := m != `{}`; offered != want {
			t.Fatalf("managed %s: web_search offered %v", m, offered)
		}
		if strings.Contains(body, "127.0.0.1:9/sink") {
			t.Fatal("the sink endpoint reached the model request")
		}
	}
}

// With no host list, web_fetch asks an embedded agent's approver, as it asks
// at the terminal; rpc and acp run on this agent.
func TestEmbeddedWebFetchAsksWithoutAHostList(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := configDir(t, `{"enabled":true}`)
	var mu sync.Mutex
	var asked []string
	a, err := abhed.New(context.Background(), abhed.Options{
		Workspace: dir, ConfigDir: dir, WorkspaceTrust: config.TrustGranted, ConfiguredTools: true,
		Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: fetchingModel(t, "https://docs.example.invalid/", nil),
			Model: "m", ContextWindow: 8192},
		Approve: func(_ context.Context, tool string, _ json.RawMessage, d abhed.Decision) (bool, error) {
			mu.Lock()
			asked = append(asked, tool+": "+d.Reason)
			mu.Unlock()
			return false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Run(context.Background(), "read it"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || !strings.HasPrefix(asked[0], "web_fetch: web_fetch asks: no allowed_hosts") {
		t.Fatalf("web_fetch with no host list was not put to the approver: %q", asked)
	}
}

// An embedded web_fetch refuses a URL carrying a stored secret, as the
// terminal's does: the tool set reads the same store.
func TestEmbeddedWebFetchRefusesAStoredSecretInTheURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	const value = "s3cret-value-9f41"
	if err := secrets.Default().Set("API_KEY", value); err != nil {
		t.Fatal(err)
	}
	dir := configDir(t, `{"enabled":true,"allowed_hosts":["docs.example.invalid"]}`)
	a, err := abhed.New(context.Background(), abhed.Options{
		Workspace: dir, ConfigDir: dir, WorkspaceTrust: config.TrustGranted, ConfiguredTools: true,
		Provider: &abhed.Provider{Type: "openai-compatible",
			BaseURL: fetchingModel(t, "https://docs.example.invalid/search?q="+value, nil), Model: "m", ContextWindow: 8192},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Run(context.Background(), "read it"); err != nil {
		t.Fatal(err)
	}
	// Refused before it runs, at the precheck, as an allowed call that cannot succeed.
	for _, ev := range a.Events() {
		if ev.Type == agent.EvObservation || ev.Type == agent.EvActionDenied {
			if !strings.Contains(string(ev.Payload), "the URL contains the stored secret") {
				t.Fatalf("web_fetch did not refuse a URL holding a stored secret: %s", ev.Payload)
			}
			return
		}
	}
	t.Fatalf("web_fetch was not refused: %v", a.Events())
}

// The embedded prompt names web_fetch only when the agent has it, with or
// without the configured tools' memory files.
func TestEmbeddedPromptNamesWebFetchOnlyWhenRegistered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const line = "web_fetch reads one page in full"
	for _, c := range []struct {
		fetch, configured, want bool
	}{{true, true, true}, {false, true, false}, {true, false, false}} {
		webFetch := ""
		if c.fetch {
			webFetch = `{"enabled":true,"allowed_hosts":["docs.example.invalid"]}`
		}
		dir := configDir(t, webFetch)
		var system atomic.Value
		a, err := abhed.New(context.Background(), abhed.Options{
			Workspace: dir, ConfigDir: dir, WorkspaceTrust: config.TrustGranted, ConfiguredTools: c.configured,
			Provider: &abhed.Provider{Type: "openai-compatible",
				BaseURL: fetchingModel(t, "https://docs.example.invalid/", &system), Model: "m", ContextWindow: 8192},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Run(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
		a.Close()
		sent, _ := system.Load().(string)
		if strings.Contains(sent, line) != c.want {
			t.Errorf("%+v: the prompt names web_fetch = %v", c, !c.want)
		}
		if !c.want && !strings.Contains(sent, "this session has no web tool") {
			t.Errorf("%+v: the prompt does not say the session has no web tool", c)
		}
	}
}
