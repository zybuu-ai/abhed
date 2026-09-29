package abhed

import (
	"context"
	"encoding/json"
	"errors"
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
)

// modelStub is a model endpoint that answers every call and counts them; a
// call blocks until release is closed when hold is set.
type modelStub struct {
	*httptest.Server
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// free lets held calls answer; a failing test frees them so Close can return.
func (st *modelStub) free() { st.once.Do(func() { close(st.release) }) }

func newModelStub(t *testing.T, answer string, hold bool) *modelStub {
	t.Helper()
	st := &modelStub{entered: make(chan struct{}, 8), release: make(chan struct{})}
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

// provider is a provider entry for a configuration file.
func provider(url, model string, extra ...string) string {
	return `{"type":"openai-compatible","base_url":"` + url + `","model":"` + model + `","context_window":8192` +
		strings.Join(extra, "") + `}`
}

// modelsEnv isolates HOME, the trust store and the managed file, and writes the
// user's own configuration, which is trusted. managedBody may be empty.
func modelsEnv(t *testing.T, userCfg, managedBody string) {
	t.Helper()
	managedFile(t, managedBody)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	t.Setenv(config.TrustEnv, "")
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(userCfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newModelsAgent(t *testing.T, ws string) *Agent {
	t.Helper()
	if ws == "" {
		ws = t.TempDir()
	}
	a, err := New(context.Background(), Options{Workspace: ws, ConfigDir: ws, AllowDefaultModel: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func TestModelsListsTheConfiguredProviders(t *testing.T) {
	a1, b1 := newModelStub(t, "a", false), newModelStub(t, "b", false)
	modelsEnv(t, `{"model":{"default":"alpha","providers":{"beta":`+provider(b1.URL, "m-b", `,"api_key_env":"ABHED_TEST_BETA_KEY"`)+
		`,"alpha":`+provider(a1.URL, "m-a")+`}}}`, "")
	got := newModelsAgent(t, "").Models()
	want := []Model{{Name: "alpha", Model: "m-a", Type: "openai-compatible", Current: true},
		{Name: "beta", Model: "m-b", Type: "openai-compatible"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Models() = %+v, want %+v (built-ins nobody configured are not offered)", got, want)
	}
}

func TestSwitchModelNamedRecordsAndMoves(t *testing.T) {
	a1, b1 := newModelStub(t, "from a", false), newModelStub(t, "from b", false)
	modelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+provider(a1.URL, "m-a")+`,"beta":`+provider(b1.URL, "m-b")+`}}}`, "")
	a := newModelsAgent(t, "")
	if err := a.SwitchModelNamed("beta"); err != nil {
		t.Fatal(err)
	}
	if out, err := a.Run(context.Background(), "hi"); err != nil || out != "from b" {
		t.Fatalf("after the switch: %q, %v; want the beta model to answer", out, err)
	}
	if a1.calls.Load() != 0 {
		t.Fatal("the old model was called after the switch")
	}
	var switched *agent.ModelSwitched
	for _, ev := range a.Events() {
		if ev.Type == EvModelSwitched {
			switched = &agent.ModelSwitched{}
			_ = json.Unmarshal(ev.Payload, switched)
		}
	}
	if switched == nil || switched.Provider != "beta" || switched.Model != "m-b" || switched.From != "m-a" {
		t.Fatalf("the record does not name the switch: %+v", switched)
	}
	if ms := a.Models(); !ms[1].Current || ms[0].Current {
		t.Fatalf("current not moved: %+v", ms)
	}
}

func TestSwitchModelNamedRefusesWhatIsNotConfigured(t *testing.T) {
	a1 := newModelStub(t, "a", false)
	modelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+provider(a1.URL, "m-a")+`}}}`, "")
	a := newModelsAgent(t, "")
	// A built-in type nobody configured, a URL, and a name that is nowhere.
	for _, name := range []string{"ollama", a1.URL, "nope"} {
		if err := a.SwitchModelNamed(name); !errors.Is(err, ErrUnknownModel) {
			t.Errorf("SwitchModelNamed(%q) = %v, want ErrUnknownModel", name, err)
		}
	}
}

func TestSwitchModelNamedRefusesDuringARun(t *testing.T) {
	a1, b1 := newModelStub(t, "a", true), newModelStub(t, "b", false)
	modelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+provider(a1.URL, "m-a")+`,"beta":`+provider(b1.URL, "m-b")+`}}}`, "")
	a := newModelsAgent(t, "")
	done := make(chan error, 1)
	go func() { _, err := a.Run(context.Background(), "hi"); done <- err }()
	select {
	case <-a1.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached the model")
	}
	if err := a.SwitchModelNamed("beta"); !errors.Is(err, ErrSwitchDuringRun) {
		t.Errorf("switch during a run: %v, want ErrSwitchDuringRun", err)
	}
	a1.free()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := a.SwitchModelNamed("beta"); err != nil {
		t.Fatalf("switch after the run: %v", err)
	}
}

// An untrusted workspace file adds no provider: it is neither listed nor reachable.
func TestUntrustedWorkspaceProviderIsNotOffered(t *testing.T) {
	a1, evil := newModelStub(t, "a", false), newModelStub(t, "evil", false)
	modelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+provider(a1.URL, "m-a")+`}}}`, "")
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	wsCfg := `{"model":{"providers":{"evil":` + provider(evil.URL, "m-evil") + `}},` +
		`"custom_providers":[{"name":"evilcorp","api":"openai","base_url":"` + evil.URL + `"}]}`
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(wsCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newModelsAgent(t, ws)
	for _, m := range a.Models() {
		if m.Name != "alpha" {
			t.Errorf("an untrusted workspace's provider is offered: %+v", m)
		}
	}
	for _, name := range []string{"evil", "evilcorp"} {
		if err := a.SwitchModelNamed(name); !errors.Is(err, ErrUnknownModel) {
			t.Errorf("SwitchModelNamed(%q) = %v, want ErrUnknownModel", name, err)
		}
	}
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if evil.calls.Load() != 0 {
		t.Fatal("the untrusted workspace's endpoint was called")
	}
}

// A provider whose key variable is unset is listed; switching to it names the
// variable, and the value of a set one never appears in an error.
func TestSwitchToAProviderWithoutItsKey(t *testing.T) {
	a1, b1 := newModelStub(t, "a", false), newModelStub(t, "b", false)
	modelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+provider(a1.URL, "m-a", `,"api_key_env":"ABHED_TEST_ALPHA_KEY"`)+
		`,"beta":`+provider(b1.URL, "m-b", `,"api_key_env":"ABHED_TEST_BETA_KEY"`)+`}}}`, "")
	t.Setenv("ABHED_TEST_ALPHA_KEY", "sk-alpha-value-123")
	t.Setenv("ABHED_TEST_BETA_KEY", "")
	a := newModelsAgent(t, "")
	if len(a.Models()) != 2 {
		t.Fatalf("a provider without its key is not listed: %+v", a.Models())
	}
	err := a.SwitchModelNamed("beta")
	if err == nil || !strings.Contains(err.Error(), "ABHED_TEST_BETA_KEY") {
		t.Fatalf("switch without the key: %v, want an error naming ABHED_TEST_BETA_KEY", err)
	}
	if strings.Contains(err.Error(), "sk-alpha-value-123") || strings.Contains(err.Error(), b1.URL) {
		t.Fatalf("the error carries a key or an endpoint: %v", err)
	}
	for _, ev := range a.Events() {
		if ev.Type == EvModelSwitched {
			t.Fatal("a refused switch was recorded")
		}
	}
}

// A managed file that sets model.default pins the model.
func TestManagedDefaultPinsTheModel(t *testing.T) {
	a1, b1 := newModelStub(t, "a", false), newModelStub(t, "b", false)
	modelsEnv(t, `{"model":{"default":"beta","providers":{"alpha":`+provider(a1.URL, "m-a")+`,"beta":`+provider(b1.URL, "m-b")+`}}}`,
		`{"model":{"default":"alpha"}}`)
	a := newModelsAgent(t, "")
	if ms := a.Models(); len(ms) != 1 || ms[0].Name != "alpha" || !ms[0].Current {
		t.Fatalf("under a managed default: %+v, want alpha only", ms)
	}
	if err := a.SwitchModelNamed("beta"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("switch away from the managed model: %v, want ErrUnknownModel", err)
	}
}

// SetModel and SwitchModelNamed from two goroutines never interleave: under
// -race an unguarded adapter write is reported, and the name left current is
// always the model the loop is on.
func TestSetModelAndSwitchModelNamedExcludeEachOther(t *testing.T) {
	a1, b1, c1 := newModelStub(t, "a", false), newModelStub(t, "b", false), newModelStub(t, "c", false)
	modelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+provider(a1.URL, "m-a")+`,"beta":`+provider(b1.URL, "m-b")+`}}}`, "")
	a := newModelsAgent(t, "")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			if err := a.SetModel(Provider{Type: "openai-compatible", BaseURL: c1.URL, Model: "m-c", ContextWindow: 8192}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 50; i++ {
		if err := a.SwitchModelNamed([]string{"alpha", "beta"}[i%2]); err != nil {
			t.Fatal(err)
		}
	}
	<-done
	a.forkMu.Lock()
	defer a.forkMu.Unlock()
	want := map[string]string{"": "m-c", "alpha": "m-a", "beta": "m-b"}[a.current]
	if got := a.loop.Adapter.Profile().Name; got != want {
		t.Fatalf("current %q names %s, but the loop is on %s", a.current, want, got)
	}
}
