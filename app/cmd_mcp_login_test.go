package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/egress/upstreamtest"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/mcpauth"
	"github.com/zybuu-ai/abhed/internal/mcpauth/mcpauthtest"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// loginHome is a home whose own configuration names the stand-in server as
// corp and a program as local, with a store of its own and no managed file
// unless managedBody is given; it returns a workspace.
func loginHome(t *testing.T, s *mcpauthtest.Server, managedBody string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(secrets.EnvFile, filepath.Join(home, ".abhed", "secrets.json"))
	t.Setenv(config.TrustEnv, "")
	old := managed.ConfigFile
	managed.ConfigFile = filepath.Join(t.TempDir(), "absent.json")
	if managedBody != "" {
		if err := os.WriteFile(managed.ConfigFile, []byte(managedBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { managed.ConfigFile = old })
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"mcp":{"servers":[{"name":"corp","url":"` + s.URL() + `","enabled":true},{"name":"local","command":"/bin/echo","enabled":true}]}}`
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	oldOpen, oldCan := openBrowser, canOpenBrowser
	openBrowser, canOpenBrowser = mcpauthtest.Browser(t), func() bool { return true }
	t.Cleanup(func() { openBrowser, canOpenBrowser = oldOpen, oldCan })
	return t.TempDir()
}

func loginIO(out, errw *bytes.Buffer) mcpIO {
	return mcpIO{in: strings.NewReader(""), out: out, err: errw, terminal: true}
}

// abhed mcp login signs in through the browser, keeps the tokens bound to
// the server, never prints them, logs the sign-in, and a session's gateway
// then reaches the server with them; logout revokes and forgets them.
func TestMCPLoginCommand(t *testing.T) {
	s := mcpauthtest.New(t)
	ws := loginHome(t, s, "")
	var out, errw bytes.Buffer
	if code := mcpCmd(ws, []string{"login", "corp"}, config.TrustRefused, loginIO(&out, &errw)); code != 0 {
		t.Fatalf("login: %d %s", code, errw.String())
	}
	if strings.Contains(out.String()+errw.String(), "access-1") || strings.Contains(out.String()+errw.String(), "refresh-1") {
		t.Fatalf("a token was printed: %s %s", out.String(), errw.String())
	}
	store := secrets.Default()
	if in, _, _, err := mcpauth.Status(store, "corp", s.URL()); !in || err != nil {
		t.Fatalf("not signed in: %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".abhed", "config-changes.jsonl"))
	if !strings.Contains(string(log), `"by":"login"`) || strings.Contains(string(log), "access-1") {
		t.Fatalf("change log %s", log)
	}
	out.Reset()
	if code := mcpCmd(ws, []string{"list"}, config.TrustRefused, loginIO(&out, &errw)); code != 0 || !strings.Contains(out.String(), "(signed in)") {
		t.Fatalf("list: %s", out.String())
	}

	// A session's gateway sends the login to the server.
	g := mcp.NewGateway()
	defer g.Close()
	g.Authorize = toolset.MCPAuthorize(store)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if errs := g.Connect(ctx, []mcp.ServerConfig{{Name: "corp", URL: s.URL(), Enabled: true}}); len(errs) > 0 {
		t.Fatalf("connect with the login: %v", errs)
	}
	if len(g.Tools()) != 1 {
		t.Fatalf("tools %v", g.Tools())
	}

	out.Reset()
	if code := mcpCmd(ws, []string{"logout", "corp"}, config.TrustRefused, loginIO(&out, &errw)); code != 0 || !strings.Contains(out.String(), "Signed out") {
		t.Fatalf("logout: %s %s", out.String(), errw.String())
	}
	if _, _, revoked := s.Get(); len(revoked) != 2 {
		t.Fatalf("revoked %v", revoked)
	}
	if in, _, _, _ := mcpauth.Status(store, "corp", s.URL()); in {
		t.Fatal("still signed in")
	}
}

// login is refused in an agent's command, without a terminal, for a program
// server, for a server not configured, and for one the MCP lists refuse.
func TestMCPLoginRefusals(t *testing.T) {
	s := mcpauthtest.New(t)
	ws := loginHome(t, s, "")
	var out, errw bytes.Buffer
	cases := map[string]struct {
		cio  mcpIO
		name string
		want string
	}{
		"in an agent":    {mcpIO{in: strings.NewReader(""), out: &out, err: &errw, terminal: true, inSandbox: true}, "corp", "inside an agent"},
		"no terminal":    {mcpIO{in: strings.NewReader(""), out: &out, err: &errw}, "corp", "terminal"},
		"a program":      {loginIO(&out, &errw), "local", "only a URL server"},
		"not configured": {loginIO(&out, &errw), "other", "no MCP server named other"},
	}
	for name, c := range cases {
		errw.Reset()
		if code := mcpCmd(ws, []string{"login", c.name}, config.TrustRefused, c.cio); code == 0 || !strings.Contains(errw.String(), c.want) {
			t.Errorf("%s: %d %s", name, code, errw.String())
		}
	}
	if _, issued, _ := s.Get(); issued != 0 {
		t.Fatalf("a refused login reached the token endpoint")
	}
	ws = loginHome(t, s, `{"mcp":{"denied":[{"name":"corp"}]}}`)
	errw.Reset()
	if code := mcpCmd(ws, []string{"login", "corp"}, config.TrustRefused, loginIO(&out, &errw)); code == 0 {
		t.Fatalf("a server the lists refuse was signed in to: %s", errw.String())
	}
}

// The sign-in goes through the corporate upstream proxy, trusting its CA
// bundle, and every request of it reaches the proxy rather than going direct.
func TestMCPLoginThroughTheUpstreamProxy(t *testing.T) {
	s := mcpauthtest.NewTLS(t)
	ws := loginHome(t, s, "")
	openBrowser = s.TLSBrowser(t)
	corp := upstreamtest.Start(t)
	egress.ResetUpstreamForTest()
	t.Cleanup(egress.ResetUpstreamForTest)
	bundle := filepath.Join(t.TempDir(), "corp.pem")
	if err := os.WriteFile(bundle, s.CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `{"network":{"upstream_proxy":{"url":"http://` + corp.Addr() + `","ca_file":"` + bundle + `"}},` +
		`"mcp":{"servers":[{"name":"corp","url":"` + s.URL() + `","enabled":true}]}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	if code := mcpCmd(ws, []string{"login", "corp"}, config.TrustRefused, loginIO(&out, &errw)); code != 0 {
		t.Fatalf("login: %d %s", code, errw.String())
	}
	asPort := s.AS.URL[strings.LastIndex(s.AS.URL, ":")+1:]
	mcpPort := s.MCP.URL[strings.LastIndex(s.MCP.URL, ":")+1:]
	if !corp.SawExactly("example.com:"+mcpPort) || !corp.SawExactly("example.com:"+asPort) {
		t.Fatal("the sign-in did not go through the upstream proxy")
	}
}

// Under the egress allowlist a sign-in reaches only what the rules allow:
// with none for the server, nothing is asked of it.
func TestMCPLoginUnderTheAllowlist(t *testing.T) {
	s := mcpauthtest.New(t)
	ws := loginHome(t, s, "")
	cfg := `{"sandbox":{"allow_network":true,"network":"allowlist"},"mcp":{"servers":[{"name":"corp","url":"` + s.URL() + `","enabled":true}]}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	if code := mcpCmd(ws, []string{"login", "corp"}, config.TrustRefused, loginIO(&out, &errw)); code == 0 {
		t.Fatal("signed in past the allowlist")
	}
	if res, issued, _ := s.Get(); issued != 0 || res != "" {
		t.Fatalf("the authorization server was reached: %q %d", res, issued)
	}
}
