package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// A managed mode binds a console session as it binds the CLI and the SDK: a
// client may narrow it to plan and choose nothing else.
func TestManagedModeBindsSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"permissions": {"mode": "default"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
	t.Setenv("HOME", t.TempDir())
	loaded, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wb := shellBench(t, func(c *config.Config) {
		auth := c.Auth
		*c = loaded
		c.Auth = auth
	})
	if !wb.s.newPolicy(policy.ModeDefault).Managed {
		t.Error("the server's engine is not marked managed")
	}
	for mode, want := range map[string]int{"auto": http.StatusForbidden, "bypass": http.StatusForbidden, "plan": http.StatusAccepted} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"workbench":true,"mode":"`+mode+`"}`))
		req.Header.Set("X-Abhed-Tenant", "acme")
		wb.h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("mode %s: %d, want %d", mode, rec.Code, want)
		}
	}
}
