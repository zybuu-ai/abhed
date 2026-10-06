package abhed

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// modelServer answers "done" and counts the requests that named model.
func modelServer(t *testing.T, model string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"`+model+`"`) {
			n.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"done"}}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// An agent built with a ConfigDir reads the ABHED_* variables, and they
// never move a model endpoint the managed file sets.
func TestManagedEndpointBeatsTheEnvironment(t *testing.T) {
	for _, pinned := range []bool{true, false} {
		corp, corpN := modelServer(t, "corp-model")
		other, otherN := modelServer(t, "other-model")
		provider := `{"model":{"default":"corp","providers":{"corp":{"type":"openai-compatible",
		  "base_url":"` + corp.URL + `","model":"corp-model","context_window":8192}}}}`
		dir := t.TempDir()
		if pinned {
			managedFile(t, provider)
		} else {
			managedFile(t, `{"permissions":{"mode":"default"}}`)
			if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".abhed"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".abhed", "config.json"), []byte(provider), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("ABHED_BASE_URL", other.URL)
		t.Setenv("ABHED_MODEL", "other-model")
		a, err := New(context.Background(), Options{Workspace: dir, ConfigDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		_, err = a.Run(context.Background(), "go")
		a.Close()
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case pinned && (corpN.Load() == 0 || otherN.Load() > 0):
			t.Errorf("managed: %d requests to the managed endpoint, %d to the environment's", corpN.Load(), otherN.Load())
		case !pinned && otherN.Load() == 0:
			t.Errorf("unmanaged: the environment no longer moves the endpoint (%d to the file's)", corpN.Load())
		}
	}
}
