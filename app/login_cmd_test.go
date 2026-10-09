package app

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/pki"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/profiles"
	"github.com/zybuu-ai/abhed/server"
)

type loginServer struct {
	ts     *httptest.Server
	cookie *http.Cookie
	ca     string
}

func newLoginServer(t *testing.T) *loginServer {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, true)
	if err := local.CreateUser(context.Background(), auth.User{Username: "ada"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.OpenCLITokens(local, filepath.Join(t.TempDir(), "k"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	login := auth.NewCLILogin(tokens)
	login.Interval = time.Second
	s := server.New(server.Options{Workspace: t.TempDir(), Config: cfg, Adapter: &fakeAdapter{name: "x"},
		Registry: tools.NewRegistry(tools.Read{}), CLILogin: login,
		Auth: &auth.Middleware{Providers: []auth.Provider{local}, Verifier: tokens,
			PublicPaths: append(append(server.PublicPaths(), local.PublicPaths()...), login.PublicPaths()...)}})
	ts := httptest.NewTLSServer(s.Handler())
	t.Cleanup(ts.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o644)
	req, _ := http.NewRequest("POST", ts.URL+"/v1/signin", strings.NewReader(`{"username":"ada","password":"correct-horse-1"}`))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	ls := &loginServer{ts: ts, ca: ca}
	for _, c := range resp.Cookies() {
		if c.Name == "abhed_session" {
			ls.cookie = c
		}
	}
	return ls
}

func (ls *loginServer) approve(code string) error {
	req, _ := http.NewRequest("POST", ls.ts.URL+auth.CLIApprovePath, strings.NewReader(`{"user_code":"`+code+`","approve":true}`))
	req.AddCookie(ls.cookie)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ls.ts.Client().Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// approver plays the person: it reads the code from the prompt and approves.
type approver struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	ls   *loginServer
	done bool
}

var codeRe = regexp.MustCompile(`\b[B-Z]{4}-[B-Z]{4}\b`)

func (a *approver) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.buf.Write(p)
	if c := codeRe.FindString(a.buf.String()); c != "" && !a.done {
		a.done = true
		go func() { time.Sleep(500 * time.Millisecond); _ = a.ls.approve(c) }()
	}
	return len(p), nil
}

func (a *approver) String() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buf.String()
}

func TestLoginProfileLogoutCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(profiles.EnvFile, "")
	ls := newLoginServer(t)
	ws := t.TempDir()
	var out bytes.Buffer
	ap := &approver{ls: ls}
	if code := loginCmd(ws, []string{"-device", "-ca", ls.ca, "-profile", "lab", ls.ts.URL}, &out, ap); code != 0 {
		t.Fatalf("login = %d\n%s%s", code, out.String(), ap.String())
	}
	if !strings.Contains(out.String(), "as ada (profile lab") {
		t.Errorf("login said %q", out.String())
	}
	path := filepath.Join(home, ".abhed", "profiles.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("profiles file mode %o, want 600", fi.Mode().Perm())
		}
	}
	var f profiles.File
	_ = json.Unmarshal(raw, &f)
	p := f.Profiles["lab"]
	if f.Current != "lab" || p == nil || !strings.HasPrefix(p.Token, auth.CLITokenPrefix) || p.Username != "ada" || p.CAFile != ls.ca {
		t.Fatalf("saved profile %+v (current %q)", p, f.Current)
	}
	for _, shown := range []string{out.String(), ap.String()} {
		if strings.Contains(shown, p.Token) {
			t.Fatal("the token was printed")
		}
	}

	out.Reset()
	var errOut bytes.Buffer
	if code := profileCmd(ws, []string{"list"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "* lab") ||
		!strings.Contains(out.String(), "signed in as ada") || strings.Contains(out.String(), p.Token) {
		t.Fatalf("profile list = %d %q %q", code, out.String(), errOut.String())
	}
	// A second server entry, then switch between them.
	f.Profiles["other"] = &profiles.Profile{URL: "https://other.test"}
	if err := profiles.Save(path, &f); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := profileCmd(ws, []string{"use", "other"}, &out, &errOut); code != 0 {
		t.Fatalf("profile use = %d", code)
	}
	if code := profileCmd(ws, []string{"use", "missing"}, &out, &errOut); code == 0 {
		t.Fatal("profile use of a missing profile succeeded")
	}
	if code := profileCmd(ws, []string{"use", "lab"}, &out, &errOut); code != 0 {
		t.Fatal("profile use lab")
	}

	// Logout ends the token on the server, not only here.
	token := p.Token
	out.Reset()
	if code := logoutCmd(ws, nil, &out, &errOut); code != 0 {
		t.Fatalf("logout = %d %s", code, errOut.String())
	}
	reg, _ := profiles.Load(path)
	if reg.Profiles["lab"].Token != "" {
		t.Fatal("logout left the token in the file")
	}
	req, _ := http.NewRequest("GET", ls.ts.URL+"/v1/cli/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := ls.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the logged-out token = %d", resp.StatusCode)
	}
	if code := profileCmd(ws, []string{"rm", "other"}, &out, &errOut); code != 0 {
		t.Fatal("profile rm")
	}
	reg, _ = profiles.Load(path)
	if _, ok := reg.Profiles["other"]; ok {
		t.Fatal("profile rm left the profile")
	}
}

func TestLoginRefusals(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out, errOut bytes.Buffer
	if code := loginCmd(t.TempDir(), []string{"http://abhed.example"}, &out, &errOut); code == 0 ||
		!strings.Contains(errOut.String(), "in clear") {
		t.Errorf("plain http login = %d %q", code, errOut.String())
	}
	// A profiles file inside the workspace is refused: commands could read it.
	ws := t.TempDir()
	t.Setenv(profiles.EnvFile, filepath.Join(ws, "profiles.json"))
	errOut.Reset()
	if code := profileCmd(ws, []string{"list"}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "inside the workspace") {
		t.Errorf("profiles in the workspace = %d %q", code, errOut.String())
	}
	t.Setenv(profiles.EnvFile, filepath.Join(ws, ".abhed", "profiles.json"))
	if code := profileCmd(ws, []string{"list"}, &out, &errOut); code != 0 {
		t.Errorf("profiles under the workspace's .abhed = %d %q", code, errOut.String())
	}
}

func TestLoginToAServerWithoutSignIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(profiles.EnvFile, "")
	s := server.New(server.Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: &fakeAdapter{name: "x"},
		Registry: tools.NewRegistry(tools.Read{})})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	var out, errOut bytes.Buffer
	if code := loginCmd(t.TempDir(), []string{ts.URL}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "no sign-in") {
		t.Fatalf("login to auth none = %d %q %q", code, out.String(), errOut.String())
	}
	// A local server that does not offer command-line sign-in says how to turn it on.
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	s = server.New(server.Options{Workspace: t.TempDir(), Config: cfg, Adapter: &fakeAdapter{name: "x"},
		Registry: tools.NewRegistry(tools.Read{}), Auth: &auth.Middleware{Providers: []auth.Provider{local},
			PublicPaths: append(server.PublicPaths(), local.PublicPaths()...)}})
	ts2 := httptest.NewServer(s.Handler())
	t.Cleanup(ts2.Close)
	errOut.Reset()
	if code := loginCmd(t.TempDir(), []string{ts2.URL}, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "auth.cli.enabled") {
		t.Fatalf("login without cli sign-in = %d %q", code, errOut.String())
	}
}

func TestBuildTLSFailsClosed(t *testing.T) {
	ws := t.TempDir()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	if l, err := buildTLS(cfg, ws, quiet); l != nil || err != nil {
		t.Fatalf("no TLS settings = %v %v", l, err)
	}
	cfg.Server.TLS.CertFile = "missing.pem"
	cfg.Server.TLS.KeyFile = "missing-key.pem"
	if _, err := buildTLS(cfg, ws, quiet); err == nil || !strings.Contains(err.Error(), "will not start") {
		t.Fatalf("missing files = %v", err)
	}
	dir := filepath.Join(t.TempDir(), "pki")
	if _, err := pki.InitCA(pki.Options{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	sc, _ := pki.IssueServer(pki.Options{Dir: dir, DNS: []string{"a.test"}})
	cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile = sc.CertFile, sc.KeyFile
	cfg.Server.TLS.MinVersion = "1.3"
	cfg.Server.TLS.ClientCAFile = filepath.Join(dir, pki.CACert)
	l, err := buildTLS(cfg, ws, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if got := tlsLabel(cfg, l); !strings.Contains(got, "TLS 1.3+") || !strings.Contains(got, "client certificates required") {
		t.Errorf("label %q", got)
	}
	if c := l.Config(); c.MinVersion != 0x0304 {
		t.Errorf("min version %x", c.MinVersion)
	}
}

func TestServeAuthWiring(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	cfg.Auth.UsersFile = filepath.Join(ws, ".abhed", "users.json")
	cfg.Server.TLS.ClientCertAccounts = true
	cfg.Auth.CLI.Enabled = true
	mw, err := newApp().buildAuth(context.Background(), cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range mw.Providers {
		names = append(names, p.Name())
	}
	if !slices.Equal(names, []string{"local", "certificate"}) {
		t.Errorf("providers %v: a cookie must be asked before a certificate", names)
	}
	login, err := buildCLILogin(cfg, ws, mw)
	if err != nil || login == nil {
		t.Fatalf("cli login = %v %v", login, err)
	}
	if mw.Verifier == nil || !slices.Contains(mw.PublicPaths, auth.CLITokenPath) || slices.Contains(mw.PublicPaths, auth.CLIApprovePath) {
		t.Errorf("verifier %v public %v", mw.Verifier, mw.PublicPaths)
	}
	key, _ := cfg.CLIKeyFile(ws)
	if !strings.HasPrefix(key, filepath.Join(os.Getenv("HOME"), ".abhed")) {
		t.Errorf("token key at %s", key)
	}
	if _, err := os.Stat(key); err != nil {
		t.Errorf("token key not created: %v", err)
	}
	cfg.Auth.CLI.Enabled = false
	if l, err := buildCLILogin(cfg, ws, mw); l != nil || err != nil {
		t.Errorf("disabled cli login = %v %v", l, err)
	}
}

type advertisingProvider struct{ auth.Provider }

func (advertisingProvider) CLILoginMethods() []auth.CLILoginMethod {
	return []auth.CLILoginMethod{{Name: "sso", TokenEndpoint: "/v1/cli/sso/token"}}
}

// With auth.mode oidc there are no local accounts: an edition's provider that
// advertises its own method serves abhed login, and without one serve refuses.
func TestCLILoginWithoutLocalAccounts(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode, cfg.Auth.CLI.Enabled = "oidc", true
	mw := &auth.Middleware{Providers: []auth.Provider{advertisingProvider{}}}
	if l, err := buildCLILogin(cfg, t.TempDir(), mw); l != nil || err != nil {
		t.Fatalf("an advertising provider = %v %v", l, err)
	}
	if mw.Verifier != nil || len(mw.PublicPaths) != 0 {
		t.Errorf("the local sign-in was wired anyway: %v %v", mw.Verifier, mw.PublicPaths)
	}
	if _, err := buildCLILogin(cfg, t.TempDir(), &auth.Middleware{}); err == nil || !strings.Contains(err.Error(), "auth.cli.enabled") {
		t.Fatalf("no provider serves it: %v", err)
	}
}

func TestServeURLScheme(t *testing.T) {
	if got := serveURL(":8443", true); got != "https://localhost:8443" {
		t.Errorf("serveURL = %q", got)
	}
	if got := serveURL("127.0.0.1:8080", false); got != "http://127.0.0.1:8080" {
		t.Errorf("serveURL = %q", got)
	}
}

func TestDoctorVerdictNamesUnusableTLS(t *testing.T) {
	var b strings.Builder
	if code := doctorVerdict(&b, configCheck{tlsDown: true}); code != 1 || !strings.Contains(b.String(), "server.tls cannot be used") {
		t.Fatalf("verdict %d: %q", code, b.String())
	}
}
