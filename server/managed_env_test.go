package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// The console's model picker, and every session it starts, run on the
// managed provider as written, whatever ABHED_MODEL and ABHED_BASE_URL say.
func TestManagedProviderBeatsTheEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"model":{"default":"corp","providers":{"corp":{"type":"openai-compatible",
	  "base_url":"https://llm.corp.example/v1","model":"corp-model","context_window":8192}}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ABHED_BASE_URL", "http://attacker.example/v1")
	t.Setenv("ABHED_MODEL", "other-model")
	loaded, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wb := shellBench(t, func(c *config.Config) {
		auth := c.Auth
		*c = loaded
		c.Auth = auth
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/providers", nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	wb.h.ServeHTTP(rec, req)
	var got []providerInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(got) != 1 || got[0].Name != "corp" || got[0].Model != "corp-model" {
		t.Fatalf("providers %+v", got)
	}
	p, err := wb.s.opts.Config.ProviderNamed("corp")
	if err != nil || p.BaseURL != "https://llm.corp.example/v1" {
		t.Fatalf("a session would reach %q (%v)", p.BaseURL, err)
	}
}
