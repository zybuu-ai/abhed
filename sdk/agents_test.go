package abhed_test

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

	"github.com/zybuu-ai/abhed/internal/managed"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// With ConfiguredTools an embedded agent offers the operator's agent
// definitions on its task tool, as the command line does.
func TestEmbeddedAgentOffersDefinitions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	old := managed.AgentsDir
	managed.AgentsDir = filepath.Join(t.TempDir(), "none")
	t.Cleanup(func() { managed.AgentsDir = old })
	dir := filepath.Join(home, ".abhed", "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auditor.md"), []byte("---\ndescription: audits the ledger\n---\nAudit.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var sent atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent.Store(string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	a, err := abhed.New(context.Background(), abhed.Options{Workspace: t.TempDir(), ConfiguredTools: true,
		Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: srv.URL, Model: "m", ContextWindow: 8192}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	body, _ := sent.Load().(string)
	if !strings.Contains(body, "auditor — audits the ledger") || !strings.Contains(body, `"auditor"`) {
		t.Fatalf("the definition is not offered to the model:\n%s", body)
	}
}
