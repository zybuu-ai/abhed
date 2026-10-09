// Package servetls is the TLS that `abhed serve` terminates itself: server
// certificates read from files and re-read when they change, a second
// certificate chosen by SNI and key type, TLS 1.2 or later with AEAD suites
// only, and optional client certificates verified against a CA.
//
// Startup fails closed: a configured certificate that cannot be read, does not
// match its key or has expired stops the server. A reload that fails keeps
// the certificates already in use, since a half-written file during rotation
// is normal and must not take the listener down.
package servetls

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Pair is one certificate chain and its key.
type Pair struct {
	CertFile string
	KeyFile  string
}

// ClientAuth says whether a client certificate is asked for.
type ClientAuth int

const (
	NoClientCert ClientAuth = iota
	// OptionalClientCert verifies a certificate the client offers and lets
	// one without a certificate through to the other sign-in methods.
	OptionalClientCert
	// RequireClientCert refuses the handshake without a verified certificate.
	RequireClientCert
)

// Spec is what the listener is built from.
type Spec struct {
	// Certs are tried in order: the first that suits the client's SNI name
	// and signature algorithms is served, and the first is the fallback.
	Certs        []Pair
	MinVersion   uint16 // tls.VersionTLS12 when zero
	ClientCAFile string
	ClientAuth   ClientAuth
	// Interval is how often the files are checked for a change; 10s when zero.
	Interval time.Duration
	Logger   *slog.Logger
	// Now is overridable by tests.
	Now func() time.Time
}

// suites are the TLS 1.2 suites offered: forward secret and AEAD only. TLS
// 1.3 suites are not configurable in Go and are all AEAD.
var suites = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
}

// Suites returns the TLS 1.2 cipher suites the listener offers.
func Suites() []uint16 { return append([]uint16(nil), suites...) }

// state is one consistent reading of every file.
type state struct {
	certs  []*tls.Certificate
	leaves []*x509.Certificate
	pool   *x509.CertPool
	digest [32]byte
}

// Listener holds the live TLS material and swaps it when the files change.
type Listener struct {
	spec    Spec
	cur     atomic.Pointer[state]
	mu      sync.Mutex // one reload at a time
	reloads atomic.Int64
	// bad is the digest of files that failed to load, so the same broken
	// files are reported once rather than at every check; guarded by mu.
	bad [32]byte
}

// New reads every file once and refuses anything unusable.
func New(spec Spec) (*Listener, error) {
	if len(spec.Certs) == 0 {
		return nil, errors.New("TLS needs a certificate and key")
	}
	if spec.MinVersion == 0 {
		spec.MinVersion = tls.VersionTLS12
	}
	if spec.MinVersion < tls.VersionTLS12 {
		return nil, errors.New("TLS below 1.2 is not offered")
	}
	if spec.ClientAuth != NoClientCert && spec.ClientCAFile == "" {
		return nil, errors.New("client certificates need a client CA file")
	}
	if spec.Interval <= 0 {
		spec.Interval = 10 * time.Second
	}
	if spec.Logger == nil {
		spec.Logger = slog.Default()
	}
	if spec.Now == nil {
		spec.Now = time.Now
	}
	l := &Listener{spec: spec}
	st, err := l.load()
	if err != nil {
		return nil, err
	}
	l.cur.Store(st)
	return l, nil
}

// Warnings lists what is allowed but unwise, for the startup banner: a key
// file other users can read, a certificate close to expiry.
func (l *Listener) Warnings() []string {
	var out []string
	for _, p := range l.spec.Certs {
		if fi, err := os.Stat(p.KeyFile); err == nil && fi.Mode().Perm()&0o077 != 0 {
			out = append(out, fmt.Sprintf("the TLS key %s is readable by other users (mode %o)", p.KeyFile, fi.Mode().Perm()))
		}
	}
	now := l.spec.Now()
	for i, c := range l.cur.Load().leaves {
		if left := c.NotAfter.Sub(now); left < 14*24*time.Hour {
			out = append(out, fmt.Sprintf("the TLS certificate %s expires in %s", l.spec.Certs[i].CertFile, left.Round(time.Hour)))
		}
	}
	return out
}

// Leaves are the server certificates in use, in configured order.
func (l *Listener) Leaves() []*x509.Certificate {
	return append([]*x509.Certificate(nil), l.cur.Load().leaves...)
}

// Reloads counts the reloads that changed what is served.
func (l *Listener) Reloads() int64 { return l.reloads.Load() }

// Config is the tls.Config to serve with. Every handshake reads the current
// material, so a reload applies to the next connection without a restart.
func (l *Listener) Config() *tls.Config {
	base := l.base()
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		c := l.base()
		st := l.cur.Load()
		c.ClientCAs = st.pool
		c.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) { return pick(st, hello), nil }
		return c, nil
	}
	return base
}

func (l *Listener) base() *tls.Config {
	c := &tls.Config{
		MinVersion:   l.spec.MinVersion,
		CipherSuites: Suites(),
		NextProtos:   []string{"h2", "http/1.1"},
	}
	st := l.cur.Load()
	c.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) { return pick(st, hello), nil }
	switch l.spec.ClientAuth {
	case OptionalClientCert:
		c.ClientAuth = tls.VerifyClientCertIfGiven
	case RequireClientCert:
		c.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return c
}

// pick serves the first certificate the client can use, by name and by key
// type; with none, the first, so the client reports a clear name mismatch.
func pick(st *state, hello *tls.ClientHelloInfo) *tls.Certificate {
	for _, c := range st.certs {
		if hello.SupportsCertificate(c) == nil {
			return c
		}
	}
	return st.certs[0]
}

// Run checks the files every Interval until ctx ends.
func (l *Listener) Run(ctx context.Context) {
	t := time.NewTicker(l.spec.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := l.Reload(); err != nil {
				l.spec.Logger.Error("TLS files changed but could not be used; still serving the previous certificates", "err", err)
			}
		}
	}
}

// Reload reads the files again and swaps them in when they changed and are
// usable. It reports whether anything changed; on error nothing does.
func (l *Listener) Reload() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, err := l.digest()
	if err != nil {
		return false, err
	}
	if d == l.cur.Load().digest || d == l.bad {
		return false, nil
	}
	st, err := l.load()
	if err != nil {
		l.bad = d
		return false, err
	}
	l.cur.Store(st)
	l.reloads.Add(1)
	subjects := make([]string, len(st.leaves))
	for i, c := range st.leaves {
		subjects[i] = fmt.Sprintf("%s (until %s)", c.Subject.CommonName, c.NotAfter.UTC().Format(time.DateOnly))
	}
	l.spec.Logger.Info("TLS certificates reloaded", "certificates", subjects)
	return true, nil
}

func (l *Listener) files() []string {
	var out []string
	for _, p := range l.spec.Certs {
		out = append(out, p.CertFile, p.KeyFile)
	}
	if l.spec.ClientCAFile != "" {
		out = append(out, l.spec.ClientCAFile)
	}
	return out
}

func (l *Listener) digest() ([32]byte, error) {
	h := sha256.New()
	for _, f := range l.files() {
		b, err := os.ReadFile(f) // #nosec G304 -- the files server.tls names
		if err != nil {
			return [32]byte{}, err
		}
		sum := sha256.Sum256(b)
		h.Write(sum[:])
	}
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d, nil
}

// load reads and checks everything. The digest is taken from the same bytes
// that are parsed, so a file replaced mid-read is caught on the next check.
func (l *Listener) load() (*state, error) {
	h := sha256.New()
	read := func(f string) ([]byte, error) {
		b, err := os.ReadFile(f) // #nosec G304 -- the files server.tls names
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		h.Write(sum[:])
		return b, nil
	}
	st := &state{}
	now := l.spec.Now()
	for _, p := range l.spec.Certs {
		if p.CertFile == "" || p.KeyFile == "" {
			return nil, errors.New("each TLS certificate needs both a certificate file and a key file")
		}
		cb, err := read(p.CertFile)
		if err != nil {
			return nil, fmt.Errorf("TLS certificate: %w", err)
		}
		kb, err := read(p.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("TLS key: %w", err)
		}
		pair, err := tls.X509KeyPair(cb, kb)
		if err != nil {
			// The error names no key material; X509KeyPair never quotes it.
			return nil, fmt.Errorf("TLS certificate %s with key %s: %w", p.CertFile, p.KeyFile, err)
		}
		leaf := pair.Leaf
		if leaf == nil {
			if leaf, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
				return nil, fmt.Errorf("TLS certificate %s: %w", p.CertFile, err)
			}
			pair.Leaf = leaf
		}
		if now.After(leaf.NotAfter) {
			return nil, fmt.Errorf("TLS certificate %s expired on %s", p.CertFile, leaf.NotAfter.UTC().Format(time.RFC3339))
		}
		if now.Before(leaf.NotBefore) {
			return nil, fmt.Errorf("TLS certificate %s is not valid until %s", p.CertFile, leaf.NotBefore.UTC().Format(time.RFC3339))
		}
		if leaf.IsCA && len(leaf.ExtKeyUsage) == 0 {
			return nil, fmt.Errorf("TLS certificate %s is a CA certificate, not a server certificate", p.CertFile)
		}
		if !serverUsage(leaf) {
			return nil, fmt.Errorf("TLS certificate %s is not for server authentication", p.CertFile)
		}
		st.certs = append(st.certs, &pair)
		st.leaves = append(st.leaves, leaf)
	}
	if l.spec.ClientCAFile != "" {
		b, err := read(l.spec.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("TLS client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) || !hasCert(b) {
			return nil, fmt.Errorf("TLS client CA %s holds no certificate", l.spec.ClientCAFile)
		}
		st.pool = pool
	}
	copy(st.digest[:], h.Sum(nil))
	return st, nil
}

func serverUsage(c *x509.Certificate) bool {
	if len(c.ExtKeyUsage) == 0 {
		return true
	}
	for _, u := range c.ExtKeyUsage {
		if u == x509.ExtKeyUsageServerAuth || u == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

func hasCert(b []byte) bool { return bytes.Contains(b, []byte("-----BEGIN CERTIFICATE-----")) }
