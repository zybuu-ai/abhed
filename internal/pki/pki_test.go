package pki

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func readCert(t *testing.T, p string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCAServerAndClientChainAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	ca, err := InitCA(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := IssueServer(Options{Dir: dir, DNS: []string{"abhed.test"}, IPs: []net.IP{net.ParseIP("127.0.0.1")}})
	if err != nil {
		t.Fatal(err)
	}
	cli, err := IssueClient(Options{Dir: dir, Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if m := mode(t, dir); m != 0o700 {
			t.Errorf("pki dir mode %o, want 700", m)
		}
		for _, k := range []string{ca.KeyFile, srv.KeyFile, cli.KeyFile} {
			if m := mode(t, k); m != 0o600 {
				t.Errorf("%s mode %o, want 600", k, m)
			}
		}
		for _, c := range []string{ca.CertFile, srv.CertFile, cli.CertFile} {
			if m := mode(t, c); m != 0o644 {
				t.Errorf("%s mode %o, want 644", c, m)
			}
		}
	}
	root := readCert(t, ca.CertFile)
	if !root.IsCA || !root.MaxPathLenZero {
		t.Error("the CA may sign other CAs")
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	s := readCert(t, srv.CertFile)
	if _, err := s.Verify(x509.VerifyOptions{Roots: pool, DNSName: "abhed.test", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Errorf("server certificate does not verify for its name: %v", err)
	}
	if _, err := s.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
		t.Error("a server certificate verified as a client certificate")
	}
	if time.Until(s.NotAfter) > ServerValidity+time.Hour {
		t.Errorf("server certificate lasts until %s, beyond what browsers accept", s.NotAfter)
	}
	c := readCert(t, cli.CertFile)
	if c.Subject.CommonName != "alice" {
		t.Errorf("client CN %q", c.Subject.CommonName)
	}
	if _, err := c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client certificate does not verify: %v", err)
	}
	if _, err := tls.LoadX509KeyPair(srv.CertFile, srv.KeyFile); err != nil {
		t.Errorf("server pair does not load: %v", err)
	}
	if len(srv.Fingerprint) != 64 {
		t.Errorf("fingerprint %q", srv.Fingerprint)
	}
}

func TestNothingIsOverwrittenWithoutForce(t *testing.T) {
	dir := t.TempDir()
	if _, err := InitCA(Options{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, CAKey))
	if _, err := InitCA(Options{Dir: dir}); !errors.Is(err, ErrExists) {
		t.Fatalf("second init = %v, want ErrExists", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, CAKey))
	if string(before) != string(after) {
		t.Fatal("the CA key was replaced")
	}
	if _, err := IssueServer(Options{Dir: dir, DNS: []string{"a.test"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := IssueServer(Options{Dir: dir, DNS: []string{"a.test"}}); !errors.Is(err, ErrExists) {
		t.Fatalf("second server = %v, want ErrExists", err)
	}
	if _, err := IssueServer(Options{Dir: dir, DNS: []string{"a.test"}, Force: true}); err != nil {
		t.Fatalf("forced reissue: %v", err)
	}
}

func TestRSAKeysAndRefusals(t *testing.T) {
	dir := t.TempDir()
	if _, err := InitCA(Options{Dir: dir, Key: RSA}); err != nil {
		t.Fatal(err)
	}
	srv, err := IssueServer(Options{Dir: dir, DNS: []string{"r.test"}, Key: RSA, Name: "rsa"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(srv.CertFile, srv.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pair.PrivateKey.(*rsa.PrivateKey); !ok {
		t.Errorf("key is %T, want RSA", pair.PrivateKey)
	}
	ec, _ := IssueServer(Options{Dir: dir, DNS: []string{"e.test"}, Name: "ec"})
	pair, _ = tls.LoadX509KeyPair(ec.CertFile, ec.KeyFile)
	if _, ok := pair.PrivateKey.(*ecdsa.PrivateKey); !ok {
		t.Errorf("default key is %T, want ECDSA", pair.PrivateKey)
	}

	for _, sub := range []string{"", "a", "../etc", "has space", strings.Repeat("x", 65)} {
		if _, err := IssueClient(Options{Dir: dir, Subject: sub}); err == nil {
			t.Errorf("client subject %q accepted", sub)
		}
	}
	if _, err := IssueServer(Options{Dir: dir}); err == nil {
		t.Error("a server certificate with no name was issued")
	}
	if _, err := IssueServer(Options{Dir: dir, DNS: []string{"x.test"}, Name: "../escape"}); err == nil {
		t.Error("a file name with a path was accepted")
	}
	if _, err := IssueServer(Options{Dir: dir, DNS: []string{"x.test"}, Name: "ca"}); err == nil {
		t.Error("a server certificate could replace the CA's files")
	}
	if _, err := InitCA(Options{Dir: t.TempDir(), Key: "dsa"}); err == nil {
		t.Error("unknown key type accepted")
	}
}

func TestLeafNeverOutlivesItsCA(t *testing.T) {
	dir := t.TempDir()
	if _, err := InitCA(Options{Dir: dir, Days: 30}); err != nil {
		t.Fatal(err)
	}
	cli, err := IssueClient(Options{Dir: dir, Subject: "bob", Days: 400})
	if err != nil {
		t.Fatal(err)
	}
	ca := readCert(t, filepath.Join(dir, CACert))
	if c := readCert(t, cli.CertFile); c.NotAfter.After(ca.NotAfter) {
		t.Errorf("leaf until %s, CA until %s", c.NotAfter, ca.NotAfter)
	}
}

func TestCAKeyOpenToOthersIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not describe access on Windows")
	}
	dir := t.TempDir()
	if _, err := InitCA(Options{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, CAKey), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := IssueClient(Options{Dir: dir, Subject: "carol"}); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("issue with a 0644 CA key = %v", err)
	}
	open := filepath.Join(t.TempDir(), "open")
	if err := os.MkdirAll(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := InitCA(Options{Dir: open}); err == nil {
		t.Error("a CA was written into a directory other users can write")
	}
}
