// Package mcpauthtest is a stand-in MCP server that wants an OAuth sign-in,
// and its authorization server on another origin, both on loopback, for
// tests of abhed mcp login and of the tokens' use.
package mcpauthtest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Server is an MCP server and its authorization server on another
// origin, both on loopback. It keeps what it was asked, to check each step.
type Server struct {
	t        *testing.T
	MCP, AS  *httptest.Server
	mu       sync.Mutex
	codes    map[string]string // code -> challenge
	access   string
	refresh  string
	issued   int
	revoked  []string
	resource string
	// Host, when set, names both servers in every URL the stub gives.
	Host string
	// knobs
	NoS256        bool
	NoRegister    bool
	WrongIssuer   bool
	WrongResource bool
	ISS           bool
	DenyRefresh   bool
	asked         []string
	ClientSecret  string
}

func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{t: t, codes: map[string]string{}}
	s.AS = httptest.NewServer(http.HandlerFunc(s.serveAS))
	s.MCP = httptest.NewServer(http.HandlerFunc(s.serveMCP))
	t.Cleanup(s.AS.Close)
	t.Cleanup(s.MCP.Close)
	return s
}

// Get reads the stub's state under its lock.
func (s *Server) Get() (resource string, issued int, revoked []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resource, s.issued, append([]string(nil), s.revoked...)
}

func (s *Server) note(what string) {
	s.mu.Lock()
	s.asked = append(s.asked, what)
	s.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource/mcp":
		s.note("prm")
		res := s.mcpBase() + "/mcp"
		if s.WrongResource {
			res = "https://elsewhere.example/mcp"
		}
		writeJSON(w, 200, map[string]any{"resource": res, "authorization_servers": []string{s.asBase()}, "scopes_supported": []string{"read"}})
	case "/mcp":
		s.mu.Lock()
		ok := s.access != "" && r.Header.Get("Authorization") == "Bearer "+s.access
		s.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+s.mcpBase()+`/.well-known/oauth-protected-resource/mcp", scope="read write"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Signed in: a minimal MCP server with one tool.
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		result := `{}`
		switch req.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			result = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}`
		case "tools/list":
			result = `{"tools":[{"name":"whoami","description":"who you are"}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveAS(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server":
		s.note("asmeta")
		issuer := s.asBase()
		if s.WrongIssuer {
			issuer = "https://evil.example"
		}
		methods := []string{"S256"}
		if s.NoS256 {
			methods = []string{"plain"}
		}
		meta := map[string]any{"issuer": issuer, "authorization_endpoint": s.asBase() + "/authorize", "token_endpoint": s.asBase() + "/token",
			"revocation_endpoint": s.asBase() + "/revoke", "code_challenge_methods_supported": methods, "response_types_supported": []string{"code"},
			"authorization_response_iss_parameter_supported": s.ISS}
		if !s.NoRegister {
			meta["registration_endpoint"] = s.asBase() + "/register"
		}
		writeJSON(w, 200, meta)
	case "/register":
		s.note("register")
		var body struct {
			RedirectURIs []string `json:"redirect_uris"`
			AuthMethod   string   `json:"token_endpoint_auth_method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.RedirectURIs) != 1 || !strings.HasPrefix(body.RedirectURIs[0], "http://127.0.0.1:") || body.AuthMethod != "none" {
			writeJSON(w, 400, map[string]string{"error": "invalid_redirect_uri"})
			return
		}
		writeJSON(w, 201, map[string]string{"client_id": "cid-1"})
	case "/authorize":
		q := r.URL.Query()
		s.mu.Lock()
		s.resource = q.Get("resource")
		s.mu.Unlock()
		if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("client_id") == "" {
			http.Error(w, "bad request", 400)
			return
		}
		code := fmt.Sprintf("code-%d", time.Now().UnixNano())
		s.mu.Lock()
		s.codes[code] = q.Get("code_challenge")
		s.mu.Unlock()
		back, _ := url.Parse(q.Get("redirect_uri"))
		bq := back.Query()
		bq.Set("code", code)
		bq.Set("state", q.Get("state"))
		if s.ISS {
			bq.Set("iss", s.asBase())
		}
		back.RawQuery = bq.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	case "/token":
		_ = r.ParseForm()
		f := r.PostForm
		if f.Get("resource") != s.mcpBase()+"/mcp" {
			writeJSON(w, 400, map[string]string{"error": "invalid_target"})
			return
		}
		if s.ClientSecret != "" && f.Get("client_secret") != s.ClientSecret {
			writeJSON(w, 401, map[string]string{"error": "invalid_client"})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		switch f.Get("grant_type") {
		case "authorization_code":
			challenge, ok := s.codes[f.Get("code")]
			delete(s.codes, f.Get("code"))
			sum := sha256.Sum256([]byte(f.Get("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
				return
			}
		case "refresh_token":
			if s.DenyRefresh || f.Get("refresh_token") != s.refresh {
				writeJSON(w, 400, map[string]string{"error": "invalid_grant", "error_description": "gone"})
				return
			}
		default:
			writeJSON(w, 400, map[string]string{"error": "unsupported_grant_type"})
			return
		}
		s.issued++
		s.access, s.refresh = fmt.Sprintf("access-%d", s.issued), fmt.Sprintf("refresh-%d", s.issued)
		writeJSON(w, 200, map[string]any{"access_token": s.access, "token_type": "Bearer", "expires_in": 3600, "refresh_token": s.refresh})
	case "/revoke":
		_ = r.ParseForm()
		s.mu.Lock()
		s.revoked = append(s.revoked, r.PostForm.Get("token"))
		s.mu.Unlock()
		w.WriteHeader(200)
	default:
		http.NotFound(w, r)
	}
}

// Browser plays the person's browser: it follows the authorization
// endpoint's redirect to the loopback callback.
func Browser(t *testing.T) func(string) error { return browserWith(t, http.DefaultTransport) }

func browserWith(t *testing.T, tr http.RoundTripper) func(string) error {
	return func(u string) error {
		go func() {
			c := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := c.Get(u)
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
			if loc := resp.Header.Get("Location"); loc != "" {
				r2, err := http.Get(loc) //nolint:noctx // the test's browser, back to the loopback callback
				if err == nil {
					_, _ = io.Copy(io.Discard, r2.Body)
					_ = r2.Body.Close()
				}
			}
		}()
		return nil
	}
}

// SetDenyRefresh makes the token endpoint refuse every refresh.
func (s *Server) SetDenyRefresh(deny bool) {
	s.mu.Lock()
	s.DenyRefresh = deny
	s.mu.Unlock()
}

// URL is the MCP server's address, as a configuration names it.
func (s *Server) URL() string { return s.mcpBase() + "/mcp" }

// mcpBase and asBase are the two servers' addresses as the stub names them:
// with Host set, that name in place of the loopback address.
func (s *Server) mcpBase() string { return s.named(s.MCP.URL) }
func (s *Server) asBase() string  { return s.named(s.AS.URL) }

func (s *Server) named(u string) string {
	if s.Host == "" {
		return u
	}
	return strings.Replace(u, "127.0.0.1", s.Host, 1)
}

// NewTLS is New over TLS, naming both servers example.com, the name the
// test certificate is for: reached only through a proxy that dials loopback.
func NewTLS(t *testing.T) *Server {
	t.Helper()
	s := &Server{t: t, codes: map[string]string{}, Host: "example.com"}
	s.AS = httptest.NewTLSServer(http.HandlerFunc(s.serveAS))
	s.MCP = httptest.NewTLSServer(http.HandlerFunc(s.serveMCP))
	t.Cleanup(s.AS.Close)
	t.Cleanup(s.MCP.Close)
	return s
}

// CertPEM is the TLS servers' certificate, for a CA bundle.
func (s *Server) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.MCP.Certificate().Raw})
}

// TLSBrowser is Browser for NewTLS: it reaches example.com on loopback and
// trusts the test certificate.
func (s *Server) TLSBrowser(t *testing.T) func(string) error {
	tr := s.MCP.Client().Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, _ := net.SplitHostPort(addr)
		var d net.Dialer
		return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}
	return browserWith(t, tr)
}
