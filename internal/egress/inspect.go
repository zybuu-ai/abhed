package egress

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"

	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// inspector terminates commands' HTTPS with leaf certificates from a CA made
// for this proxy alone, whose key never leaves the process.
type inspector struct {
	ca    *x509.Certificate
	key   *ecdsa.PrivateKey
	pem   []byte
	roots *x509.CertPool // upstream verification; nil is the system's

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// maxLeaves bounds the leaf certificates a proxy keeps.
const maxLeaves = 1024

func newInspector(roots *x509.CertPool) (*inspector, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Abhed egress inspection (one session)", Organization: []string{"Abhed"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(7 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &inspector{ca: ca, key: key, roots: roots, leaves: map[string]*tls.Certificate{},
		pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// leaf is a certificate for host signed by the proxy's CA.
func (in *inspector) leaf(host string) (*tls.Certificate, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if c := in.leaves[host]; c != nil {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     in.ca.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if a, err := netip.ParseAddr(host); err == nil {
		tmpl.IPAddresses = []net.IP{a.AsSlice()}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, in.ca, &key.PublicKey, in.key)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, in.ca.Raw}, PrivateKey: key}
	if len(in.leaves) >= maxLeaves {
		in.leaves = map[string]*tls.Certificate{}
	}
	in.leaves[host] = c
	return c, nil
}

// Inspecting reports whether the proxy terminates commands' HTTPS.
func (p *Proxy) Inspecting() bool { return p.ins != nil }

// CA is the PEM of the proxy's own CA, or nil when it does not inspect.
func (p *Proxy) CA() []byte {
	if p.ins == nil {
		return nil
	}
	return append([]byte(nil), p.ins.pem...)
}

// systemBundles are where systems keep their CA bundle as one file.
var systemBundles = []string{"/etc/ssl/cert.pem", "/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem", "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"}

// CABundle is the system's CA bundle with the proxy's CA after it, for a
// command's clients to trust in place of the system's alone.
func (p *Proxy) CABundle() []byte {
	var b []byte
	for _, f := range systemBundles {
		if data, err := os.ReadFile(f); err == nil && len(data) > 0 { // #nosec G304 -- a fixed system path
			b = append(b, data...)
			b = append(b, '\n')
			break
		}
	}
	return append(b, p.CA()...)
}

// SetCAPath names where commands find CABundle, as they see the path; their
// clients are then pointed at it.
func (p *Proxy) SetCAPath(path string) { p.caPath.Store(&path) }

// caEnv points the usual clients at the CA bundle.
func (p *Proxy) caEnv() []string {
	pp := p.caPath.Load()
	if p.ins == nil || pp == nil || *pp == "" {
		return nil
	}
	f := *pp
	return []string{"SSL_CERT_FILE=" + f, "CURL_CA_BUNDLE=" + f, "REQUESTS_CA_BUNDLE=" + f, "GIT_SSL_CAINFO=" + f,
		"PIP_CERT=" + f, "NODE_EXTRA_CA_CERTS=" + f, "AWS_CA_BUNDLE=" + f, "CARGO_HTTP_CAINFO=" + f}
}

// prefixed reads what a bufio.Reader holds before the connection's own bytes.
type prefixed struct {
	net.Conn
	r *bufio.Reader
}

func (c *prefixed) Read(b []byte) (int, error) { return c.r.Read(b) }

// inspectTunnel terminates a CONNECT's TLS and judges each request inside by
// method and path; a client not speaking TLS is judged by host and port.
func (p *Proxy) inspectTunnel(c net.Conn, br *bufio.Reader, up *counted, t Target, callID string, ev *Event) {
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(headerTimeout))
	first, err := br.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		return
	}
	if first[0] != 0x16 { // not a TLS handshake: nothing to look inside
		v := p.opts.Policy.Decide(t.Request())
		if !v.Allowed() {
			ev.Decision, ev.Rule, ev.Reason = v.Decision, v.Rule, v.Reason+"; the tunnel does not carry TLS, so it is judged by host and port"
			return
		}
		ev.Reason += "; not TLS, passed through as a tunnel"
		p.tunnel(c, br, up, false)
		return
	}
	ev.Reason += "; TLS inspected"
	srv := tls.Server(&prefixed{Conn: c, r: br}, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if h.ServerName != "" && !strings.EqualFold(strings.TrimSuffix(h.ServerName, "."), t.Host) {
				return nil, fmt.Errorf("the TLS name %q is not the tunnel's host", h.ServerName)
			}
			return p.ins.leaf(t.Host)
		},
	})
	_ = srv.SetDeadline(time.Now().Add(headerTimeout))
	if err := srv.Handshake(); err != nil {
		p.record(Event{CallID: callID, Kind: "https", Host: t.Host, Port: t.Port, Decision: Deny, Rule: "tls",
			Reason: "the client's TLS handshake failed (does it trust the session's CA?): " + err.Error()})
		return
	}
	_ = srv.SetDeadline(time.Time{})
	// An address is verified against the certificate's IP names.
	conf := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.ins.roots, NextProtos: []string{"http/1.1"}, ServerName: t.Host}
	upTLS := tls.Client(up, conf)
	_ = upTLS.SetDeadline(time.Now().Add(headerTimeout))
	if err := upTLS.Handshake(); err != nil {
		p.record(Event{CallID: callID, Kind: "https", Host: t.Host, Port: t.Port, Decision: Deny, Rule: "tls",
			Reason: "the server's certificate could not be verified: " + err.Error()})
		writeStatus(srv, http.StatusBadGateway, "abhed egress: the server's certificate could not be verified\n", nil)
		return
	}
	_ = upTLS.SetDeadline(time.Time{})
	cr, ur := bufio.NewReader(srv), bufio.NewReader(upTLS)
	authority := net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port)))
	for {
		_ = srv.SetReadDeadline(time.Now().Add(p.idle()))
		req, err := http.ReadRequest(cr)
		if err != nil {
			return
		}
		_ = srv.SetReadDeadline(time.Time{})
		if !p.inspectOne(srv, ur, upTLS, req, t, authority, callID) || req.Close {
			return
		}
	}
}

// inspectOne judges one request inside an inspected tunnel and, allowed,
// forwards it and its response; false ends the connection.
func (p *Proxy) inspectOne(srv net.Conn, ur *bufio.Reader, upTLS net.Conn, req *http.Request, t Target, authority, callID string) bool {
	e := Event{CallID: callID, Kind: "https", Host: t.Host, Port: t.Port, Method: req.Method}
	refuse := func(code int, rule, why string) bool {
		e.Decision, e.Rule, e.Reason = Deny, rule, why
		p.record(e)
		writeStatus(srv, code, "abhed egress: "+why+"\n", nil)
		return false
	}
	if !strings.HasPrefix(req.RequestURI, "/") {
		return refuse(http.StatusBadRequest, "parse", "a request inside the tunnel needs a path, not "+req.RequestURI)
	}
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if !strings.EqualFold(strings.TrimSuffix(strings.Trim(host, "[]"), "."), t.Host) {
		return refuse(http.StatusForbidden, "host", "the request names the host "+req.Host+", not the tunnel's "+t.Host)
	}
	if req.Header.Get("Upgrade") != "" {
		return refuse(http.StatusForbidden, "upgrade", "a protocol upgrade cannot be inspected")
	}
	inner, err := parseAbsolute(req.Method, "http://"+authority+req.RequestURI)
	if err != nil {
		return refuse(http.StatusBadRequest, "parse", err.Error())
	}
	e.Path = inner.Path
	v := p.opts.Policy.Decide(Request{Host: t.Host, Port: t.Port, Method: req.Method, Path: inner.Path})
	e.Decision, e.Rule, e.Reason = v.Decision, v.Rule, v.Reason
	if !v.Allowed() {
		p.record(e)
		writeStatus(srv, http.StatusForbidden, fmt.Sprintf("abhed egress: %s %s refused by %s: %s\n", req.Method, inner.Path, v.Rule, v.Reason), nil)
		return false
	}
	p.record(e)
	for _, f := range strings.Split(req.Header.Get("Connection"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			req.Header.Del(f)
		}
	}
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	req.URL.Scheme, req.URL.Host = "", ""
	req.RequestURI = ""
	// A client waiting for 100 Continue would never send the body the write
	// below reads: the proxy, having decided, answers it itself.
	if strings.EqualFold(strings.TrimSpace(req.Header.Get("Expect")), "100-continue") {
		req.Header.Del("Expect")
		if _, err := io.WriteString(srv, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			return false
		}
	}
	if err := req.Write(upTLS); err != nil {
		writeStatus(srv, http.StatusBadGateway, "abhed egress: sending the request failed\n", nil)
		return false
	}
	resp, err := http.ReadResponse(ur, req)
	// Interim responses go on to the client; the loop waits for the final one.
	for err == nil && resp.StatusCode >= 100 && resp.StatusCode < 200 && resp.StatusCode != http.StatusSwitchingProtocols {
		for _, h := range hopHeaders {
			resp.Header.Del(h)
		}
		if _, werr := fmt.Fprintf(srv, "HTTP/1.1 %s\r\n", resp.Status); werr != nil {
			return false
		}
		if werr := resp.Header.Write(srv); werr != nil {
			return false
		}
		if _, werr := io.WriteString(srv, "\r\n"); werr != nil {
			return false
		}
		resp, err = http.ReadResponse(ur, req)
	}
	if err != nil || resp.StatusCode == http.StatusSwitchingProtocols {
		writeStatus(srv, http.StatusBadGateway, "abhed egress: the upstream response was malformed\n", nil)
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	if err := resp.Write(srv); err != nil {
		return false
	}
	return !resp.Close
}

// idle is how long an inspected connection waits for its next request.
func (p *Proxy) idle() time.Duration {
	if p.opts.Idle > 0 {
		return p.opts.Idle
	}
	return p.opts.Policy.Idle()
}
