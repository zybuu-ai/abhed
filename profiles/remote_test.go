package profiles

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/server"
)

func TestProfilesFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home", ".abhed", "profiles.json")
	f, err := Load(path)
	if err != nil || len(f.Profiles) != 0 {
		t.Fatalf("missing file = %v %v", f, err)
	}
	f.Profiles["prod"] = &Profile{URL: "https://abhed.test", Token: "abhcli_secret", Username: "ada"}
	f.Current = "prod"
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("profiles mode %o, want 600", fi.Mode().Perm())
		}
		di, _ := os.Stat(filepath.Dir(path))
		if di.Mode().Perm() != 0o700 {
			t.Errorf("profiles dir mode %o, want 700", di.Mode().Perm())
		}
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, p, err := got.Get(""); err != nil || p.Token != "abhcli_secret" {
		t.Fatalf("current profile = %v %v", p, err)
	}
	if _, _, err := got.Get("other"); err == nil {
		t.Error("a missing profile was found")
	}
	if runtime.GOOS == "windows" {
		return
	}
	// Opened up by someone: refused rather than read.
	_ = os.Chmod(path, 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a 0644 profiles file = %v", err)
	}
	_ = os.Chmod(path, 0o600)
	link := filepath.Join(filepath.Dir(path), "link.json")
	_ = os.Symlink(path, link)
	if _, err := Load(link); err == nil {
		t.Error("a profiles file behind a link was read")
	}
	if err := Save(link, got); err == nil {
		t.Error("a profiles file was written through a link")
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://abhed.test/":      "https://abhed.test",
		"https://abhed.test:8443":  "https://abhed.test:8443",
		"http://127.0.0.1:8860":    "http://127.0.0.1:8860",
		"http://localhost:8860/":   "http://localhost:8860",
		"https://abhed.test/team/": "https://abhed.test/team",
	} {
		if got, err := NormalizeURL(in, false); err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"http://abhed.test", "ftp://abhed.test", "abhed.test", "https://u:p@abhed.test", "https://abhed.test/?x=1"} {
		if _, err := NormalizeURL(in, false); err == nil {
			t.Errorf("NormalizeURL(%q) accepted", in)
		}
	}
	if _, err := NormalizeURL("http://abhed.test", true); err != nil {
		t.Errorf("-allow-http refused: %v", err)
	}
	if _, err := Resolve("https://a.test", "http://idp.test/token"); err == nil {
		t.Error("a plain-http endpoint on another host was accepted")
	}
	if got, _ := Resolve("https://a.test", "/v1/cli/token"); got != "https://a.test/v1/cli/token" {
		t.Errorf("relative endpoint = %q", got)
	}
}

type stub struct{}

func (stub) Name() string                           { return "stub" }
func (stub) Profile() model.Profile                 { return model.Profile{Name: "stub", ContextWindow: 32000} }
func (stub) CountTokens(model.Request) (int, error) { return 1, nil }
func (stub) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk)
	close(ch)
	return ch, nil
}

type rig struct {
	srv    *httptest.Server
	local  *auth.LocalAuth
	cookie *http.Cookie
	ca     string
}

// newRig is a TLS server with local accounts and command-line sign-in.
func newRig(t *testing.T) *rig {
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
	s := server.New(server.Options{Workspace: t.TempDir(), Config: cfg, Adapter: stub{},
		Registry: tools.NewRegistry(tools.Read{}), CLILogin: login,
		Auth: &auth.Middleware{Providers: []auth.Provider{local}, Verifier: tokens,
			PublicPaths: append(append(server.PublicPaths(), local.PublicPaths()...), login.PublicPaths()...)}})
	ts := httptest.NewTLSServer(s.Handler())
	t.Cleanup(ts.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	writePEM(t, ca, ts)
	r := &rig{srv: ts, local: local, ca: ca}
	req, _ := http.NewRequest("POST", ts.URL+"/v1/signin", strings.NewReader(`{"username":"ada","password":"correct-horse-1"}`))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "abhed_session" {
			r.cookie = c
		}
	}
	if r.cookie == nil {
		t.Fatal("no session")
	}
	return r
}

// browser acts as the signed-in person: same origin, the session cookie.
func (r *rig) browser(t *testing.T, method, path, body string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	req.AddCookie(r.cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", r.srv.URL)
	// Called from the goroutine playing the browser, so it reports, never stops.
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Error(err)
		return map[string]any{"redirect": "http://127.0.0.1:1/"}
	}
	defer func() { _ = resp.Body.Close() }()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	if resp.StatusCode != 200 {
		t.Errorf("%s %s = %d %v", method, path, resp.StatusCode, m)
	}
	return m
}

func TestDeviceLoginAgainstTheServer(t *testing.T) {
	r := newRig(t)
	p := &Profile{URL: r.srv.URL, CAFile: r.ca}
	c, err := p.HTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	d, err := Discover(context.Background(), c, p.URL)
	if err != nil || d.AuthMode != "local" || len(d.Methods) != 1 {
		t.Fatalf("discover = %+v %v", d, err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	tok, err := DeviceLogin(context.Background(), c, p.URL, d.Methods[0], "test", func(code, uri, complete string) {
		if !strings.HasPrefix(uri, r.srv.URL+"/cli/login") || !strings.Contains(complete, url.QueryEscape(code)) {
			t.Errorf("prompt %s %s", uri, complete)
		}
		go func() {
			defer wg.Done()
			time.Sleep(1500 * time.Millisecond)
			r.browser(t, "POST", auth.CLIApprovePath, `{"user_code":"`+code+`","approve":true}`)
		}()
	})
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	p.Apply(tok, time.Now())
	me, err := Whoami(context.Background(), c, p)
	if err != nil || me["owner"] != "local:ada" {
		t.Fatalf("whoami = %v %v", me, err)
	}
	p.RevocationEndpoint = r.srv.URL + auth.CLIRevokePath
	if err := Revoke(context.Background(), c, p); err != nil {
		t.Fatal(err)
	}
	if _, err := Whoami(context.Background(), c, p); err == nil {
		t.Fatal("the token still works after logout")
	}
}

func TestDeviceLoginDenied(t *testing.T) {
	r := newRig(t)
	p := &Profile{URL: r.srv.URL, CAFile: r.ca}
	c, _ := p.HTTPClient()
	d, _ := Discover(context.Background(), c, p.URL)
	_, err := DeviceLogin(context.Background(), c, p.URL, d.Methods[0], "test", func(code, _, _ string) {
		go r.browser(t, "POST", auth.CLIApprovePath, `{"user_code":"`+code+`","approve":false}`)
	})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("denied sign-in = %v", err)
	}
}

func TestPKCELoginAgainstTheServer(t *testing.T) {
	r := newRig(t)
	p := &Profile{URL: r.srv.URL, CAFile: r.ca}
	c, _ := p.HTTPClient()
	d, _ := Discover(context.Background(), c, p.URL)
	// The browser: approve on the page's endpoint, then follow the redirect.
	open := func(u string) error {
		pu, err := url.Parse(u)
		if err != nil {
			return err
		}
		q := pu.Query()
		if pu.Path != auth.CLIPagePath || q.Get("code_challenge_method") != "S256" {
			t.Errorf("authorization URL %s", u)
		}
		go func() {
			b, _ := json.Marshal(map[string]any{"code_challenge": q.Get("code_challenge"), "code_challenge_method": "S256",
				"redirect_uri": q.Get("redirect_uri"), "state": q.Get("state"), "approve": true})
			m := r.browser(t, "POST", auth.CLIAuthorizePath, string(b))
			// A forged callback with the wrong state is turned away.
			bad, _ := url.Parse(q.Get("redirect_uri"))
			bad.RawQuery = url.Values{"code": {"x"}, "state": {"wrong"}}.Encode()
			if resp, err := http.Get(bad.String()); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("forged callback = %d", resp.StatusCode)
				}
			}
			resp, err := http.Get(m["redirect"].(string))
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
		}()
		return nil
	}
	tok, err := PKCELogin(context.Background(), c, p.URL, d.Methods[0], open)
	if err != nil {
		t.Fatal(err)
	}
	p.Apply(tok, time.Now())
	if me, err := Whoami(context.Background(), c, p); err != nil || me["subject"] != "ada" {
		t.Fatalf("whoami = %v %v", me, err)
	}
}

func TestClientRefusesAnUntrustedServer(t *testing.T) {
	r := newRig(t)
	p := &Profile{URL: r.srv.URL} // no CA: the test server's is not trusted
	c, _ := p.HTTPClient()
	if _, err := Discover(context.Background(), c, p.URL); err == nil {
		t.Fatal("an unverified certificate was accepted")
	}
}

func writePEM(t *testing.T, path string, ts *httptest.Server) {
	t.Helper()
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
