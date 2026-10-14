package app

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/pki"
)

// expiredPair writes a self-signed server certificate that expired yesterday.
func expiredPair(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "old.test"},
		DNSNames:     []string{"old.test"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, keyFile := filepath.Join(dir, "old.pem"), filepath.Join(dir, "old-key.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, keyFile
}

// serve -check refuses every server.tls setup serve would refuse at start.
func TestServeCheckTLS(t *testing.T) {
	ws := checkEnv(t, `{}`, "")
	// Under the home directory's .abhed, which no command can reach.
	dir := filepath.Join(os.Getenv("HOME"), ".abhed", "pki")
	if _, err := pki.InitCA(pki.Options{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	a, err := pki.IssueServer(pki.Options{Dir: dir, Name: "a", DNS: []string{"a.test"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := pki.IssueServer(pki.Options{Dir: dir, Name: "b", DNS: []string{"b.test"}})
	if err != nil {
		t.Fatal(err)
	}
	oldCert, oldKey := expiredPair(t, dir)
	ca := filepath.Join(dir, pki.CACert)

	run := func(tlsCfg map[string]any) (int, string) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"server": map[string]any{"tls": tlsCfg}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".abhed", "config.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		code := newApp().serveCheck(&out, ws, "127.0.0.1:0")
		return code, out.String()
	}

	for name, tc := range map[string]struct {
		cfg  map[string]any
		want string
	}{
		"mismatched pair": {map[string]any{"cert_file": a.CertFile, "key_file": b.KeyFile}, "private key does not match"},
		"missing files":   {map[string]any{"cert_file": filepath.Join(dir, "none.pem"), "key_file": filepath.Join(dir, "none-key.pem")}, "no such file"},
		"expired":         {map[string]any{"cert_file": oldCert, "key_file": oldKey}, "expired on"},
		"missing client CA": {map[string]any{"cert_file": a.CertFile, "key_file": a.KeyFile,
			"client_ca_file": filepath.Join(dir, "none-ca.pem")}, "TLS client CA"},
		"bad extra pair": {map[string]any{"cert_file": a.CertFile, "key_file": a.KeyFile,
			"additional_certificates": []map[string]string{{"cert_file": b.CertFile, "key_file": a.KeyFile}}}, "private key does not match"},
	} {
		code, out := run(tc.cfg)
		if code != 1 || !strings.Contains(out, "FAIL tls") || !strings.Contains(out, tc.want) || strings.Contains(out, "Ready to serve.") {
			t.Errorf("%s: exit %d:\n%s", name, code, out)
		}
	}

	code, out := run(map[string]any{"cert_file": a.CertFile, "key_file": a.KeyFile, "min_version": "1.3",
		"client_ca_file": ca, "client_auth": "optional", "reload_seconds": 30})
	if code != 0 || !strings.Contains(out, "Ready to serve.") {
		t.Fatalf("good pair: exit %d:\n%s", code, out)
	}
	for _, want := range []string{"ok   tls", "TLS 1.3+", "client certificates optional"} {
		if !strings.Contains(out, want) {
			t.Errorf("good pair: output lacks %q:\n%s", want, out)
		}
	}

	// Without server.tls the line says plain HTTP.
	if code, out := run(map[string]any{}); code != 0 || !strings.Contains(out, "off (plain HTTP") {
		t.Errorf("no TLS: exit %d:\n%s", code, out)
	}
}
