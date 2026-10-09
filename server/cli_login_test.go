package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/pki"
	"github.com/zybuu-ai/abhed/internal/servetls"
	"github.com/zybuu-ai/abhed/internal/tools"
)

type cliRig struct {
	srv    *Server
	h      http.Handler
	local  *auth.LocalAuth
	tokens *auth.CLITokens
	login  *auth.CLILogin
	mw     *auth.Middleware
}

func newCLIRig(t *testing.T, extra ...auth.Provider) *cliRig {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	if err := local.CreateUser(context.Background(), auth.User{Username: "alice"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.OpenCLITokens(local, filepath.Join(t.TempDir(), "cli.key"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	login := auth.NewCLILogin(tokens)
	mw := &auth.Middleware{Providers: append([]auth.Provider{local}, extra...), Verifier: tokens,
		PublicPaths: append(append(PublicPaths(), local.PublicPaths()...), login.PublicPaths()...)}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Auth: mw, CLILogin: login})
	return &cliRig{srv: s, h: s.Handler(), local: local, tokens: tokens, login: login, mw: mw}
}

func (c *cliRig) do(cookie *http.Cookie, method, path, body string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec
}

func (c *cliRig) signIn(t *testing.T) *http.Cookie {
	t.Helper()
	rec := c.do(nil, "POST", "/v1/signin", `{"username":"alice","password":"correct-horse-1"}`)
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == "abhed_session" {
			return ck
		}
	}
	t.Fatalf("sign-in = %d %s", rec.Code, rec.Body)
	return nil
}

type idpProvider struct{ stubProvider }

func (idpProvider) CLILoginMethods() []auth.CLILoginMethod {
	return []auth.CLILoginMethod{{Name: "oidc", DeviceAuthorizationEndpoint: "https://idp.example/device",
		TokenEndpoint: "https://idp.example/token", ClientID: "abhed-cli", Issuer: "https://idp.example"}}
}

func TestCLIDiscovery(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(rec, httptest.NewRequest("GET", CLIDiscoveryPath, nil))
	var d struct {
		AuthMode string                `json:"auth_mode"`
		Methods  []auth.CLILoginMethod `json:"methods"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if rec.Code != 200 || d.AuthMode != "none" || len(d.Methods) != 0 {
		t.Fatalf("no-auth discovery = %d %s", rec.Code, rec.Body)
	}
	// Answered before sign-in, and lists an edition provider's own method after the local one.
	r := newCLIRig(t, idpProvider{})
	rec = r.do(nil, "GET", CLIDiscoveryPath, "")
	d.Methods = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if rec.Code != 200 || d.AuthMode != "local" || len(d.Methods) != 2 ||
		d.Methods[0].Name != "local" || d.Methods[0].TokenEndpoint != auth.CLITokenPath || d.Methods[1].ClientID != "abhed-cli" {
		t.Fatalf("local discovery = %d %s", rec.Code, rec.Body)
	}
}

func TestCLILoginPageAndCSRF(t *testing.T) {
	r := newCLIRig(t)
	rec := r.do(nil, "GET", auth.CLIPagePath+"?user_code=BCDF-GHJK", "", "Accept", "text/html")
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "return=") {
		t.Fatalf("page without a session = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	cookie := r.signIn(t)
	rec = r.do(cookie, "GET", auth.CLIPagePath, "", "Accept", "text/html")
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		!strings.Contains(rec.Body.String(), "/v1/cli/approve") {
		t.Fatalf("page = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "http://") || strings.Contains(rec.Body.String(), "https://") {
		t.Error("the page links outside this server")
	}

	start := r.do(nil, "POST", auth.CLIDevicePath, "")
	var d map[string]any
	_ = json.Unmarshal(start.Body.Bytes(), &d)
	body := `{"user_code":"` + d["user_code"].(string) + `","approve":true}`
	if rec := r.do(cookie, "POST", auth.CLIApprovePath, body, "Content-Type", "application/json",
		"Origin", "https://evil.example"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin approve = %d", rec.Code)
	}
	if rec := r.do(cookie, "POST", auth.CLIApprovePath, body, "Content-Type", "application/json",
		"Origin", "http://example.com"); rec.Code != http.StatusOK {
		t.Fatalf("same-origin approve = %d %s", rec.Code, rec.Body)
	}
}

func TestCLIEndpointsAreRateLimited(t *testing.T) {
	r := newCLIRig(t)
	limited := false
	for i := 0; i < 70 && !limited; i++ {
		rec := r.do(nil, "POST", auth.CLITokenPath, "grant_type=x", "Content-Type", "application/x-www-form-urlencoded")
		limited = rec.Code == http.StatusTooManyRequests
	}
	if !limited {
		t.Fatal("the token endpoint was never throttled")
	}
	// An edition's sign-in under /v1/cli/, and token exchange, share the budget.
	for _, path := range []string{"/v1/cli/sso/token", TokenExchangePath} {
		if rec := r.do(nil, "POST", path, "grant_type=x", "Content-Type", "application/x-www-form-urlencoded"); rec.Code != http.StatusTooManyRequests {
			t.Errorf("%s after the limit = %d", path, rec.Code)
		}
	}
	if rec := r.do(nil, "GET", "/v1/cli/whoami", ""); rec.Code == http.StatusTooManyRequests {
		t.Error("whoami is throttled with the sign-in endpoints")
	}
	// Signing in by password has its own budget.
	if rec := r.do(nil, "POST", "/v1/signin", `{"username":"alice","password":"correct-horse-1"}`); rec.Code != 200 {
		t.Errorf("sign-in after the CLI limit = %d", rec.Code)
	}
}

func TestCLITokenReachesTheAPI(t *testing.T) {
	r := newCLIRig(t)
	tok, _, err := r.tokens.Issue(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	rec := r.do(nil, "GET", "/v1/cli/whoami", "", "Authorization", "Bearer "+tok)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"owner":"local:alice"`) {
		t.Fatalf("whoami with a token = %d %s", rec.Code, rec.Body)
	}
	if rec := r.do(nil, "GET", "/v1/sessions", "", "Authorization", "Bearer "+tok); rec.Code != 200 {
		t.Fatalf("sessions with a token = %d", rec.Code)
	}
	if rec := r.do(nil, "POST", auth.CLIRevokePath, "", "Authorization", "Bearer "+tok); rec.Code != 200 {
		t.Fatalf("logout = %d %s", rec.Code, rec.Body)
	}
	if rec := r.do(nil, "GET", "/v1/sessions", "", "Authorization", "Bearer "+tok); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sessions after logout = %d", rec.Code)
	}
	if rec := r.do(nil, "GET", "/v1/sessions", ""); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), "sign in required") {
		t.Fatalf("no credentials = %d %s", rec.Code, rec.Body)
	}
}

// The whole listener: TLS from files, a client certificate mapped to an
// account, and plain HTTP refused on the same port.
func TestServeTLSWithClientCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	if _, err := pki.InitCA(pki.Options{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	sc, err := pki.IssueServer(pki.Options{Dir: dir, DNS: []string{"abhed.test"}, IPs: []net.IP{net.ParseIP("127.0.0.1")}})
	if err != nil {
		t.Fatal(err)
	}
	cc, err := pki.IssueClient(pki.Options{Dir: dir, Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := servetls.New(servetls.Spec{Certs: []servetls.Pair{{CertFile: sc.CertFile, KeyFile: sc.KeyFile}},
		ClientCAFile: filepath.Join(dir, pki.CACert), ClientAuth: servetls.OptionalClientCert,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, true)
	if err := local.CreateUser(context.Background(), auth.User{Username: "alice"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), TLS: l.Config(),
		Auth: &auth.Middleware{Providers: []auth.Provider{local, auth.NewClientCertAuth(local)},
			PublicPaths: append(PublicPaths(), local.PublicPaths()...)}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("the server did not stop")
		}
	})
	base := "https://" + ln.Addr().String()

	caPEM, _ := os.ReadFile(filepath.Join(dir, pki.CACert))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	client := func(withCert bool) *http.Client {
		tc := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		if withCert {
			pair, err := tls.LoadX509KeyPair(cc.CertFile, cc.KeyFile)
			if err != nil {
				t.Fatal(err)
			}
			tc.Certificates = []tls.Certificate{pair}
		}
		return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true}}
	}
	resp, err := client(true).Get(base + "/v1/cli/whoami")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"owner":"local:alice"`) {
		t.Fatalf("whoami with a client certificate = %d %s", resp.StatusCode, b)
	}
	if resp.ProtoMajor != 2 {
		t.Errorf("served over HTTP/%d, want h2", resp.ProtoMajor)
	}
	resp, err = client(false).Get(base + "/v1/cli/whoami")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("whoami without a certificate = %d", resp.StatusCode)
	}
	resp, err = client(false).Get(base + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("health over TLS = %d", resp.StatusCode)
	}
	// Plain HTTP on the TLS port gets no API answer.
	plain := &http.Client{Timeout: 5 * time.Second}
	resp, err = plain.Get("http://" + ln.Addr().String() + "/v1/health")
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == 200 || strings.Contains(string(b), `"status"`) {
			t.Errorf("plain HTTP was served the API: %d %s", resp.StatusCode, b)
		}
	} else if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "malformed") {
		t.Logf("plain HTTP: %v", err)
	}
}
