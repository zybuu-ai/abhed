package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/pki"
)

// certServer serves h over TLS, asking for client certificates from the CA in
// dir; certificates the TLS layer cannot verify never reach h.
func certServer(t *testing.T, dir string, h http.Handler) *httptest.Server {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, pki.CACert))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(b)
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{ClientCAs: pool, ClientAuth: tls.VerifyClientCertIfGiven, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func clientFor(t *testing.T, srv *httptest.Server, dir, user string) *http.Client {
	t.Helper()
	c := srv.Client()
	tr := c.Transport.(*http.Transport).Clone()
	if user != "" {
		got, err := pki.IssueClient(pki.Options{Dir: dir, Subject: user, Force: true})
		if err != nil {
			t.Fatal(err)
		}
		pair, err := tls.LoadX509KeyPair(got.CertFile, got.KeyFile)
		if err != nil {
			t.Fatal(err)
		}
		tr.TLSClientConfig.Certificates = []tls.Certificate{pair}
	}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func TestClientCertificateSignsTheAccountIn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	if _, err := pki.InitCA(pki.Options{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	local := newTestAuth(t)
	mustCreate(t, local, "ada", "correct-horse-battery")
	certs := NewClientCertAuth(local)
	mw := Middleware{Providers: []Provider{local, certs}}
	srv := certServer(t, dir, mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := FromContext(r.Context())
		_ = json.NewEncoder(w).Encode(map[string]string{"owner": id.Owner(), "subject": id.Subject})
	})))
	get := func(c *http.Client) (int, map[string]string) {
		t.Helper()
		resp, err := c.Get(srv.URL + "/x")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var m map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	code, m := get(clientFor(t, srv, dir, "ada"))
	if code != 200 || m["owner"] != "local:ada" {
		t.Fatalf("ada's certificate = %d %v", code, m)
	}
	if code, _ := get(clientFor(t, srv, dir, "")); code != http.StatusUnauthorized {
		t.Errorf("no certificate = %d", code)
	}
	if code, _ := get(clientFor(t, srv, dir, "nobody")); code != http.StatusUnauthorized {
		t.Errorf("a certificate for no account = %d", code)
	}

	// Removing the account ends the certificate's access within the recheck.
	if _, err := RemoveUser(context.Background(), local.Store, "ada"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(certRecheck + 100*time.Millisecond)
	if code, _ := get(clientFor(t, srv, dir, "ada")); code != http.StatusUnauthorized {
		t.Errorf("a removed account's certificate = %d", code)
	}

	mustCreate(t, local, "ada", "correct-horse-battery")
	local.Admit = func(context.Context, *User) error { return errors.New("no seat") }
	time.Sleep(certRecheck + 100*time.Millisecond)
	if code, _ := get(clientFor(t, srv, dir, "ada")); code != http.StatusUnauthorized {
		t.Errorf("Admit was not asked for a certificate = %d", code)
	}
}

func TestUnverifiedCertificateNamesNoOne(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if CertSubject(r) != "" {
		t.Fatal("plain HTTP named a subject")
	}
	// Offered but not verified: PeerCertificates without VerifiedChains.
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
	if CertSubject(r) != "" {
		t.Fatal("an unverified certificate named a subject")
	}
}
