package profiles

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// NormalizeURL checks a server URL and drops a trailing slash. Plain http is
// refused for anything but this machine unless allowHTTP: a token sent in
// clear can be read by anyone on the path.
func NormalizeURL(raw string, allowHTTP bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("%q is not a server URL; use https://host[:port]", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q: give the server's URL alone, with no user, query or fragment", raw)
	}
	if u.Scheme == "http" && !allowHTTP && !Loopback(u.Hostname()) {
		return "", fmt.Errorf("%s is plain http, which would send the sign-in token in clear; use https, or -allow-http on a network you trust", raw)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// Loopback reports whether host names this machine.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// TLSConfig is the client TLS a profile asks for: TLS 1.2 or later, its CA
// alone when it names one, and its client certificate when it has one.
func (p *Profile) TLSConfig() (*tls.Config, error) {
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if p.CAFile != "" {
		b, err := os.ReadFile(p.CAFile)
		if err != nil {
			return nil, fmt.Errorf("CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("CA file %s holds no certificate", p.CAFile)
		}
		c.RootCAs = pool
	}
	if p.CertFile != "" || p.KeyFile != "" {
		if p.CertFile == "" || p.KeyFile == "" {
			return nil, errors.New("a client certificate needs both its certificate and key files")
		}
		pair, err := tls.LoadX509KeyPair(p.CertFile, p.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		c.Certificates = []tls.Certificate{pair}
	}
	return c, nil
}

// HTTPClient is a client for this profile's server. It never follows a
// redirect to another host, so the token is not carried off.
func (p *Profile) HTTPClient() (*http.Client, error) {
	tc, err := p.TLSConfig()
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	tr.Proxy = http.ProxyFromEnvironment
	return &http.Client{Transport: tr, Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				return http.ErrUseLastResponse
			}
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		}}, nil
}

// NewRequest builds a request to path on the profile's server, carrying its
// token when it holds one.
func (p *Profile) NewRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, p.URL+path, body)
	if err != nil {
		return nil, err
	}
	if p.Token != "" {
		typ := p.TokenType
		if typ == "" || strings.EqualFold(typ, "bearer") {
			typ = "Bearer"
		}
		req.Header.Set("Authorization", typ+" "+p.Token)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}
