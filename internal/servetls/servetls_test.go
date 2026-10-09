package servetls

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/pki"
)

type fixture struct {
	dir  string
	pool *x509.CertPool
}

func newPKI(t *testing.T) fixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pki")
	if _, err := pki.InitCA(pki.Options{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, pki.CACert))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(b)
	return fixture{dir: dir, pool: pool}
}

func (f fixture) server(t *testing.T, name string, key pki.KeyType, dns ...string) Pair {
	t.Helper()
	got, err := pki.IssueServer(pki.Options{Dir: f.dir, Name: name, Key: key, DNS: dns, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	return Pair{CertFile: got.CertFile, KeyFile: got.KeyFile}
}

func (f fixture) client(t *testing.T, user string) tls.Certificate {
	t.Helper()
	got, err := pki.IssueClient(pki.Options{Dir: f.dir, Subject: user, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	c, err := tls.LoadX509KeyPair(got.CertFile, got.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// serve accepts TLS connections with l's config and completes handshakes.
func serve(t *testing.T, l *Listener) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", l.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_ = c.(*tls.Conn).Handshake()
				_, _ = c.Write([]byte("ok"))
			}()
		}
	}()
	return ln.Addr().String()
}

func dial(addr string, cfg *tls.Config) (*tls.ConnectionState, error) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	c, err := tls.DialWithDialer(d, "tcp", addr, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	// A refused client certificate surfaces on the first read in TLS 1.3.
	buf := make([]byte, 2)
	if _, err := io.ReadFull(c, buf); err != nil {
		return nil, err
	}
	st := c.ConnectionState()
	return &st, nil
}

func TestHandshakeVersionsAndSuites(t *testing.T) {
	f := newPKI(t)
	l, err := New(Spec{Certs: []Pair{f.server(t, "a", pki.ECDSA, "a.test")}, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	addr := serve(t, l)
	st, err := dial(addr, &tls.Config{RootCAs: f.pool, ServerName: "a.test", NextProtos: []string{"h2", "http/1.1"}})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if st.Version != tls.VersionTLS13 {
		t.Errorf("negotiated %x, want TLS 1.3 by default", st.Version)
	}
	if st.NegotiatedProtocol != "h2" {
		t.Errorf("ALPN %q, want h2 offered", st.NegotiatedProtocol)
	}
	if _, err := dial(addr, &tls.Config{RootCAs: f.pool, ServerName: "a.test", MaxVersion: tls.VersionTLS11, MinVersion: tls.VersionTLS10}); err == nil {
		t.Error("a TLS 1.1 client was served")
	}
	st, err = dial(addr, &tls.Config{RootCAs: f.pool, ServerName: "a.test", MaxVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("TLS 1.2: %v", err)
	}
	found := false
	for _, s := range Suites() {
		found = found || s == st.CipherSuite
	}
	if !found {
		t.Errorf("TLS 1.2 negotiated %s, not one of the offered suites", tls.CipherSuiteName(st.CipherSuite))
	}
	// A client that only knows CBC suites finds nothing in common.
	if _, err := dial(addr, &tls.Config{RootCAs: f.pool, ServerName: "a.test", MaxVersion: tls.VersionTLS12,
		CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA}}); err == nil {
		t.Error("a CBC-only TLS 1.2 client was served")
	}

	l13, err := New(Spec{Certs: []Pair{f.server(t, "b", pki.ECDSA, "b.test")}, MinVersion: tls.VersionTLS13, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dial(serve(t, l13), &tls.Config{RootCAs: f.pool, ServerName: "b.test", MaxVersion: tls.VersionTLS12}); err == nil {
		t.Error("min_version 1.3 served a TLS 1.2 client")
	}
	if _, err := New(Spec{Certs: []Pair{f.server(t, "c", pki.ECDSA, "c.test")}, MinVersion: tls.VersionTLS11}); err == nil {
		t.Error("a TLS 1.1 minimum was accepted")
	}
}

func TestSNIPicksByNameAndKeyType(t *testing.T) {
	f := newPKI(t)
	ec := f.server(t, "ec", pki.ECDSA, "one.test", "both.test")
	rs := f.server(t, "rs", pki.RSA, "two.test", "both.test")
	l, err := New(Spec{Certs: []Pair{ec, rs}, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	addr := serve(t, l)
	leafKey := func(st *tls.ConnectionState) any { return st.PeerCertificates[0].PublicKey }

	st, err := dial(addr, &tls.Config{RootCAs: f.pool, ServerName: "one.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := leafKey(st).(*ecdsa.PublicKey); !ok {
		t.Errorf("one.test served %T", leafKey(st))
	}
	st, err = dial(addr, &tls.Config{RootCAs: f.pool, ServerName: "two.test"})
	if err != nil {
		t.Fatalf("second name: %v", err)
	}
	if _, ok := leafKey(st).(*rsa.PublicKey); !ok {
		t.Errorf("two.test served %T, want the RSA certificate", leafKey(st))
	}
	// Same name on both: a TLS 1.2 client that can only verify RSA gets RSA.
	st, err = dial(addr, &tls.Config{RootCAs: f.pool, ServerName: "both.test", MaxVersion: tls.VersionTLS12,
		CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}})
	if err != nil {
		t.Fatalf("RSA-only client: %v", err)
	}
	if _, ok := leafKey(st).(*rsa.PublicKey); !ok {
		t.Errorf("an RSA-only client was served %T", leafKey(st))
	}
	st, _ = dial(addr, &tls.Config{RootCAs: f.pool, ServerName: "both.test"})
	if _, ok := leafKey(st).(*ecdsa.PublicKey); !ok {
		t.Errorf("a modern client was served %T, want the first certificate", leafKey(st))
	}
}

func TestReloadSwapsAndKeepsServingOnBadFiles(t *testing.T) {
	f := newPKI(t)
	p := f.server(t, "live", pki.ECDSA, "r.test")
	var logs bytes.Buffer
	l, err := New(Spec{Certs: []Pair{p}, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	addr := serve(t, l)
	serial := func() string {
		t.Helper()
		st, err := dial(addr, &tls.Config{RootCAs: f.pool, ServerName: "r.test"})
		if err != nil {
			t.Fatal(err)
		}
		return st.PeerCertificates[0].SerialNumber.String()
	}
	first := serial()
	if changed, err := l.Reload(); changed || err != nil {
		t.Fatalf("reload with nothing changed = %v, %v", changed, err)
	}
	f.server(t, "live", pki.ECDSA, "r.test") // rotated in place
	if changed, err := l.Reload(); !changed || err != nil {
		t.Fatalf("reload after rotation = %v, %v", changed, err)
	}
	second := serial()
	if second == first {
		t.Fatal("the rotated certificate is not served")
	}

	// A half-written certificate keeps the old one in service, reported once.
	if err := os.WriteFile(p.CertFile, []byte("-----BEGIN CERTIFICATE-----\ntruncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reload(); err == nil {
		t.Fatal("a broken certificate reloaded")
	}
	if _, err := l.Reload(); err != nil {
		t.Errorf("the same broken files were reported again: %v", err)
	}
	if got := serial(); got != second {
		t.Fatal("a failed reload changed what is served")
	}
	// A key that does not match its certificate is refused the same way.
	other := f.server(t, "other", pki.ECDSA, "r.test")
	ob, _ := os.ReadFile(other.CertFile)
	_ = os.WriteFile(p.CertFile, ob, 0o644)
	if _, err := l.Reload(); err == nil {
		t.Fatal("a mismatched key reloaded")
	}
	if l.Reloads() != 1 {
		t.Errorf("reloads = %d, want 1", l.Reloads())
	}
	if !strings.Contains(logs.String(), "TLS certificates reloaded") {
		t.Errorf("the reload was not logged: %s", logs.String())
	}
}

func TestStartupFailsClosed(t *testing.T) {
	f := newPKI(t)
	good := f.server(t, "good", pki.ECDSA, "g.test")
	other := f.server(t, "other", pki.ECDSA, "g.test")
	caPEM := filepath.Join(f.dir, pki.CACert)
	empty := filepath.Join(t.TempDir(), "empty.pem")
	_ = os.WriteFile(empty, []byte("nothing here"), 0o644)
	cases := map[string]Spec{
		"no certificate":       {},
		"missing cert file":    {Certs: []Pair{{CertFile: good.CertFile + ".gone", KeyFile: good.KeyFile}}},
		"missing key file":     {Certs: []Pair{{CertFile: good.CertFile, KeyFile: good.KeyFile + ".gone"}}},
		"key from another":     {Certs: []Pair{{CertFile: good.CertFile, KeyFile: other.KeyFile}}},
		"half a pair":          {Certs: []Pair{good, {CertFile: good.CertFile}}},
		"expired":              {Certs: []Pair{good}, Now: func() time.Time { return time.Now().Add(3 * 365 * 24 * time.Hour) }},
		"not yet valid":        {Certs: []Pair{good}, Now: func() time.Time { return time.Now().Add(-24 * time.Hour) }},
		"CA as server":         {Certs: []Pair{{CertFile: caPEM, KeyFile: filepath.Join(f.dir, pki.CAKey)}}},
		"client CA empty":      {Certs: []Pair{good}, ClientCAFile: empty, ClientAuth: RequireClientCert},
		"client auth, no CA":   {Certs: []Pair{good}, ClientAuth: OptionalClientCert},
		"client CA unreadable": {Certs: []Pair{good}, ClientCAFile: empty + ".gone", ClientAuth: RequireClientCert},
	}
	for name, spec := range cases {
		spec.Logger = quiet()
		if _, err := New(spec); err == nil {
			t.Errorf("%s: started", name)
		} else if strings.Contains(err.Error(), "PRIVATE KEY") {
			t.Errorf("%s: the error quotes key material: %v", name, err)
		}
	}
}

func TestClientCertificates(t *testing.T) {
	f := newPKI(t)
	srv := f.server(t, "m", pki.ECDSA, "m.test")
	alice := f.client(t, "alice")
	stranger := newPKI(t).client(t, "alice") // same name, another CA
	ca := filepath.Join(f.dir, pki.CACert)

	req, err := New(Spec{Certs: []Pair{srv}, ClientCAFile: ca, ClientAuth: RequireClientCert, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	addr := serve(t, req)
	base := &tls.Config{RootCAs: f.pool, ServerName: "m.test"}
	if _, err := dial(addr, base); err == nil {
		t.Error("required: a client without a certificate was served")
	}
	c := base.Clone()
	c.Certificates = []tls.Certificate{stranger}
	if _, err := dial(addr, c); err == nil {
		t.Error("required: a certificate from another CA was served")
	}
	c.Certificates = []tls.Certificate{alice}
	if _, err := dial(addr, c); err != nil {
		t.Errorf("required: a valid client certificate was refused: %v", err)
	}

	opt, err := New(Spec{Certs: []Pair{srv}, ClientCAFile: ca, ClientAuth: OptionalClientCert, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	oaddr := serve(t, opt)
	if _, err := dial(oaddr, base); err != nil {
		t.Errorf("optional: a client without a certificate was refused: %v", err)
	}
	c.Certificates = []tls.Certificate{stranger}
	if _, err := dial(oaddr, c); err == nil {
		t.Error("optional: a certificate from another CA was accepted")
	}

	// Replacing the client CA ends certificates from the old one.
	next := newPKI(t)
	nb, _ := os.ReadFile(filepath.Join(next.dir, pki.CACert))
	if err := os.WriteFile(ca, nb, 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := req.Reload(); !changed || err != nil {
		t.Fatalf("client CA reload = %v, %v", changed, err)
	}
	c.Certificates = []tls.Certificate{alice}
	if _, err := dial(addr, c); err == nil {
		t.Error("a certificate from the replaced CA was still served")
	}
	c.Certificates = []tls.Certificate{next.client(t, "alice")}
	if _, err := dial(addr, c); err != nil {
		t.Errorf("a certificate from the new CA was refused: %v", err)
	}
}

func TestWarningsNameAnOpenKey(t *testing.T) {
	f := newPKI(t)
	p := f.server(t, "w", pki.ECDSA, "w.test")
	if err := os.Chmod(p.KeyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := New(Spec{Certs: []Pair{p}, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(l.Warnings(), "\n"); !strings.Contains(w, "readable by other users") {
		t.Errorf("warnings = %q", w)
	}
}
