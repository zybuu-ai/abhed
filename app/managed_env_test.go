package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// endpoint is a model server that answers "done" and keeps each request's body.
func endpoint(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		models = append(models, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"done"}}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), models...)
	}
}

// A -p run under a managed file that sets the model's endpoint goes to that
// endpoint whatever ABHED_BASE_URL and ABHED_MODEL say; without the managed
// provider the same variables move it.
func TestHeadlessRunKeepsTheManagedEndpoint(t *testing.T) {
	for _, c := range []struct {
		name   string
		pinned bool
	}{{"managed provider", true}, {"managed mode only", false}} {
		t.Run(c.name, func(t *testing.T) {
			corp, corpGot := endpoint(t)
			other, otherGot := endpoint(t)
			body := `{"permissions":{"mode":"default"}}`
			if c.pinned {
				body = `{"model":{"default":"corp","providers":{"corp":{"type":"openai-compatible",
				  "base_url":"` + corp.URL + `","model":"corp-model","context_window":8192}}}}`
			}
			managedConfig(t, body)
			if !c.pinned {
				// The user's own file names the endpoint the environment then moves.
				userConfig(t, `{"model":{"default":"corp","providers":{"corp":{"type":"openai-compatible",
				  "base_url":"`+corp.URL+`","model":"corp-model","context_window":8192}}}}`)
			}
			t.Setenv("ABHED_BASE_URL", other.URL)
			t.Setenv("ABHED_MODEL", "other-model")
			ws, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			Main([]string{"-C", ws, "-p", "go"})
			sentCorp, sentOther := corpGot(), otherGot()
			if c.pinned {
				if len(sentOther) > 0 || len(sentCorp) == 0 {
					t.Fatalf("the run went to the environment's endpoint (%d) over the managed one (%d)", len(sentOther), len(sentCorp))
				}
				if !strings.Contains(sentCorp[0], `"corp-model"`) {
					t.Errorf("the run asked for another model: %s", sentCorp[0])
				}
				return
			}
			if len(sentOther) == 0 || !strings.Contains(sentOther[0], `"other-model"`) {
				t.Fatalf("the environment no longer moves an unmanaged endpoint: %d to it, %d to the file's", len(sentOther), len(sentCorp))
			}
		})
	}
}

// userConfig writes body as the user's own configuration in the test's HOME.
func userConfig(t *testing.T, body string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".abhed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
