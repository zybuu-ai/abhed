package toolset

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/mcpauth"
	"github.com/zybuu-ai/abhed/internal/mcpauth/mcpauthtest"
	"github.com/zybuu-ai/abhed/internal/secrets"
)

// A tool set built with logins withheld, as abhed serve builds its shared
// one when accounts sign in, leaves a signed-in server unconnected and says
// why; built without, the same server connects with the login.
func TestBuildWithholdsMCPLogins(t *testing.T) {
	s := mcpauthtest.New(t)
	store := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := mcpauth.Login(ctx, store, mcpauth.LoginOptions{Server: "corp", URL: s.URL(), HTTP: noRedirectClient(),
		Open: mcpauthtest.Browser(t), Timeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.MCP.Servers = []config.MCPServerConfig{{Name: "corp", URL: s.URL(), Enabled: true}}
	for _, withhold := range []bool{true, false} {
		var warned []string
		o := Options{Workspace: t.TempDir(), Parts: MCP, Vault: store, Warn: func(f string, a ...any) {
			warned = append(warned, strings.TrimSpace(sprintf(f, a...)))
		}}
		if withhold {
			o.NoMCPLogins = ErrMCPLoginsPerAccount
		}
		set := Build(ctx, cfg, o)
		n := len(set.Gateway.Tools())
		set.Close()
		joined := strings.Join(warned, "\n")
		if withhold && (n != 0 || !strings.Contains(joined, "aren't available per account")) {
			t.Fatalf("withheld: %d tools, warned %q", n, joined)
		}
		if !withhold && n != 1 {
			t.Fatalf("with the login: %d tools, warned %q", n, joined)
		}
	}
}

func noRedirectClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }
