package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	// A server where accounts sign in shares one gateway: the operator's
	// login is withheld, and the server refused with why, not reached as the operator.
	if !accountsSignIn(config.Config{Auth: config.AuthConfig{Mode: "local"}}) || accountsSignIn(config.Config{}) {
		t.Fatal("accountsSignIn")
	}
	if held := mcpLoginsHeld(config.Config{MCP: config.MCPConfig{Servers: []config.MCPServerConfig{{Name: "corp", URL: s.URL(), Enabled: true}}}}, store); len(held) != 1 {
		t.Fatalf("held %v", held)
	}
	// abhed serve's wiring: accounts signing in withhold the logins, with a warning.
	var warned bytes.Buffer
	srvCfg := config.Config{Auth: config.AuthConfig{Mode: "oidc"}, MCP: config.MCPConfig{Servers: []config.MCPServerConfig{{Name: "corp", URL: s.URL(), Enabled: true}}}}
	if why := serveMCPLogins(srvCfg, store, &warned); !errors.Is(why, toolset.ErrMCPLoginsPerAccount) || !strings.Contains(warned.String(), "MCP server corp has an OAuth login") {
		t.Fatalf("serve with accounts: %v %q", why, warned.String())
	}
	srvCfg.Auth.Mode = "none"
	if why := serveMCPLogins(srvCfg, store, &warned); why != nil {
		t.Fatalf("serve without accounts withheld the logins: %v", why)
	}
	shared := mcp.NewGateway()
	defer shared.Close()
	shared.Authorize = toolset.MCPLoginsWithheld(store, toolset.ErrMCPLoginsPerAccount)
	_, issuedBefore, _ := s.Get()
	errs := shared.Connect(ctx, []mcp.ServerConfig{{Name: "corp", URL: s.URL(), Enabled: true}})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "aren't available per account") || len(shared.Tools()) != 0 {
		t.Fatalf("the shared gateway used the operator's login: %v", errs)
	}
	if _, issuedAfter, _ := s.Get(); issuedAfter != issuedBefore {
		t.Fatal("the withheld login was refreshed")
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

// editorURLFollower is the output of a login run by an editor: when the sign-in's
// address is printed it follows it, as the browser the editor opened would.
type editorURLFollower struct {
	t      *testing.T
	follow func(string) error
	mu     sync.Mutex
	buf    bytes.Buffer
	done   bool
}

func (u *editorURLFollower) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.buf.Write(p)
	if !u.done {
		for _, line := range strings.Split(u.buf.String(), "\n") {
			if l := strings.TrimSpace(line); strings.HasPrefix(line, "  http") && strings.Contains(l, "code_challenge") {
				u.done = true
				go func() { _ = u.follow(l) }()
			}
		}
	}
	return len(p), nil
}

func (u *editorURLFollower) String() string { u.mu.Lock(); defer u.mu.Unlock(); return u.buf.String() }

// An editor signs in with the yes on a descriptor: the browser is left to it
// (the address is printed, never opened here), the sign-in is logged as the
// editor's, and no yes signs nothing in.
func TestMCPLoginConfirmsOnADescriptor(t *testing.T) {
	s := mcpauthtest.New(t)
	ws := loginHome(t, s, "")
	follow := openBrowser
	openBrowser = func(string) error { t.Error("the engine opened a browser for an editor's sign-in"); return nil }
	run := func(answer string) (int, string) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		_, _ = w.WriteString(answer)
		_ = w.Close()
		out := &editorURLFollower{t: t, follow: follow}
		var errw bytes.Buffer
		// The argv exactly as Studio's engine host sends it: login -confirm-fd N -- NAME.
		code := mcpCmd(ws, []string{"login", "-confirm-fd", strconv.Itoa(int(r.Fd())), "--", "corp"}, config.TrustRefused,
			mcpIO{in: strings.NewReader(""), out: out, err: &errw})
		return code, out.String() + errw.String()
	}
	if code, out := run("\n"); code == 0 || !strings.Contains(out, "no yes was read") {
		t.Fatalf("no yes: %d %s", code, out)
	}
	if _, issued, _ := s.Get(); issued != 0 {
		t.Fatal("a sign-in with no yes reached the token endpoint")
	}
	code, out := run("y\n")
	if code != 0 || !strings.Contains(out, "Signed in to MCP server corp") || strings.Contains(out, "access-1") {
		t.Fatalf("login: %d %s", code, out)
	}
	log, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".abhed", "config-changes.jsonl"))
	if !strings.Contains(string(log), `"by":"login -confirm-fd"`) {
		t.Fatalf("change log %s", log)
	}
	// A descriptor that is not open is said as such.
	var errw bytes.Buffer
	if code := mcpCmd(ws, []string{"login", "-confirm-fd", "250", "--", "corp"}, config.TrustRefused, mcpIO{in: strings.NewReader(""), out: &bytes.Buffer{}, err: &errw}); code == 0 || !strings.Contains(errw.String(), "-confirm-fd 250 is not open") {
		t.Fatalf("a closed descriptor: %d %q", code, errw.String())
	}
	for _, bad := range [][]string{{"login", "-confirm-fd", "2", "corp"}, {"login", "-confirm-fd", "3"}, {"login", "a", "b"}} {
		if code := mcpCmd(ws, bad, config.TrustRefused, loginIO(&bytes.Buffer{}, &bytes.Buffer{})); code != 2 {
			t.Errorf("%v: %d", bad, code)
		}
	}
}

// After the authorization server refuses the refresh, abhed mcp list no
// longer says signed in, and Studio's view says a sign-in is wanted.
func TestMCPListAfterARefusedRefresh(t *testing.T) {
	s := mcpauthtest.New(t)
	ws := loginHome(t, s, "")
	var out, errw bytes.Buffer
	if code := mcpCmd(ws, []string{"login", "corp"}, config.TrustRefused, loginIO(&out, &errw)); code != 0 {
		t.Fatalf("login: %s", errw.String())
	}
	srv := config.MCPServerConfig{Name: "corp", URL: s.URL(), Enabled: true}
	if v := mcpAuthView(srv, false, time.Now()); v["state"] != "signed-in" || v["needsLogin"] != false || v["refreshable"] != true {
		t.Fatalf("view %v", v)
	}
	store := secrets.Default()
	a, err := mcpauth.Load(store, "corp", s.URL(), mcpOAuthHTTP(nil))
	if err != nil {
		t.Fatal(err)
	}
	s.SetDenyRefresh(true)
	if _, err := a.Refresh(context.Background(), "access-1"); err == nil {
		t.Fatal("the refresh was not refused")
	}
	out.Reset()
	if code := mcpCmd(ws, []string{"list"}, config.TrustRefused, loginIO(&out, &errw)); code != 0 {
		t.Fatal(errw.String())
	}
	if strings.Contains(out.String(), "(signed in") || !strings.Contains(out.String(), "refused at refresh") || !strings.Contains(out.String(), "abhed mcp login corp") {
		t.Fatalf("list after a refused refresh: %s", out.String())
	}
	v := mcpAuthView(srv, false, time.Now())
	if v["state"] != "refused" || v["needsLogin"] != true || v["refusedAt"] == nil {
		t.Fatalf("view after a refused refresh: %v", v)
	}
	for _, tok := range []string{"access-1", "refresh-1"} {
		if strings.Contains(fmt.Sprint(v), tok) {
			t.Fatalf("the view holds a token: %v", v)
		}
	}
	// No login, and a server that answered 401.
	if v := mcpAuthView(config.MCPServerConfig{Name: "other", URL: s.URL()}, true, time.Now()); v["state"] != "none" || v["needsLogin"] != true {
		t.Fatalf("a 401 with no login: %v", v)
	}
}
