package config

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strings"

	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/secrets"
)

// NetworkConfig governs how Abhed's own connections leave the machine.
type NetworkConfig struct {
	// UpstreamProxy is a corporate proxy every connection goes out through.
	UpstreamProxy *UpstreamProxyConfig `json:"upstream_proxy,omitempty"`
}

// UpstreamProxyConfig is an explicit HTTP proxy, reached over http or https.
// A PAC file is not read. The egress allowlist decides first; the proxy
// carries only what it allowed.
type UpstreamProxyConfig struct {
	// URL is http://host:port or https://host:port, with no user, path or query.
	URL string `json:"url"`
	// CAFile is a PEM bundle trusted beside the system's, for an https proxy
	// and for servers a proxy that inspects TLS re-signs.
	CAFile string `json:"ca_file,omitempty"`
	// CredentialsSecret names a stored secret holding user:password, sent to
	// the proxy alone.
	CredentialsSecret string `json:"credentials_secret,omitempty"`
	// NoProxy are destinations reached directly: host names (".corp" or
	// "*.corp" for a domain), IP addresses and CIDR ranges.
	NoProxy []string `json:"no_proxy,omitempty"`
	// ResolveViaProxy lets a name that does not resolve on this machine go
	// to the proxy unchecked, for a host a rule names exactly. Off, such a
	// name is refused.
	ResolveViaProxy bool `json:"resolve_via_proxy,omitempty"`
}

// clone is a deep copy, so a restore is not changed by a later merge.
func (n NetworkConfig) clone() NetworkConfig {
	if n.UpstreamProxy == nil {
		return n
	}
	p := *n.UpstreamProxy
	p.NoProxy = append([]string(nil), p.NoProxy...)
	return NetworkConfig{UpstreamProxy: &p}
}

// validateNetwork checks network.upstream_proxy as written.
func (c Config) validateNetwork() error {
	p := c.Network.UpstreamProxy
	if p == nil {
		return nil
	}
	if _, err := proxyURL(p.URL); err != nil {
		return err
	}
	if p.CAFile != "" {
		if _, err := readBundle(p.CAFile); err != nil {
			return err
		}
	}
	if p.CredentialsSecret != "" && !secrets.ValidName(p.CredentialsSecret) {
		return fmt.Errorf("network.upstream_proxy.credentials_secret %q is not a secret name", Printable(p.CredentialsSecret))
	}
	for _, e := range p.NoProxy {
		if err := noProxyEntry(e); err != nil {
			return err
		}
	}
	return nil
}

// proxyURL parses and checks the proxy's URL.
func proxyURL(raw string) (*url.URL, error) {
	const key = "network.upstream_proxy.url"
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New(key + " is empty; give http://host:port or https://host:port, or remove upstream_proxy")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s is not a URL", key)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("%s must be http:// or https:// (a SOCKS proxy or a PAC file is not supported)", key)
	case u.User != nil:
		// Not echoed: it holds a credential.
		return nil, fmt.Errorf("%s holds a user or password; store them with `abhed secret set NAME` as user:password and name it in network.upstream_proxy.credentials_secret", key)
	case u.Hostname() == "":
		return nil, fmt.Errorf("%s has no host", key)
	case !isASCII(u.Hostname()):
		return nil, fmt.Errorf("%s names its host in non-ASCII characters; write the proxy's host as ASCII (its punycode form)", key)
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return nil, fmt.Errorf("%s may name only a scheme, host and port (a PAC file is not supported)", key)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// readBundle reads the PEM certificates at path.
func readBundle(path string) ([]*x509.Certificate, error) {
	const key = "network.upstream_proxy.ca_file"
	data, err := os.ReadFile(path) // #nosec G304 -- the operator's CA bundle
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	var certs []*x509.Certificate
	for len(data) > 0 {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", key, path, err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("%s %s holds no PEM certificate", key, path)
	}
	return certs, nil
}

// noProxyEntry checks one no_proxy entry.
func noProxyEntry(e string) error {
	const key = "network.upstream_proxy.no_proxy"
	s := strings.TrimSpace(e)
	switch s {
	case "":
		return fmt.Errorf("%s has an empty entry", key)
	case "*":
		return fmt.Errorf("%s may not be *: remove upstream_proxy to reach everything directly", key)
	}
	if strings.Contains(s, "/") {
		if _, err := netip.ParsePrefix(s); err != nil {
			return fmt.Errorf("%s entry %q is not a CIDR range", key, Printable(e))
		}
		return nil
	}
	if _, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		return nil
	}
	h := strings.TrimPrefix(strings.TrimPrefix(s, "*"), ".")
	if h == "" || strings.ContainsAny(h, "*:/ \t") {
		return fmt.Errorf("%s entry %q is not a host, address or CIDR range (a port is not supported)", key, Printable(e))
	}
	return nil
}

// managedNetwork keeps a proxy the managed file sets from being widened
// below it: the person's no_proxy list goes unless the managed file names its own.
func managedNetwork(cfg *Config) {
	if cfg.Network.UpstreamProxy == nil || !cfg.ManagedSets("network.upstream_proxy") {
		return
	}
	if !cfg.ManagedSets("network.upstream_proxy.no_proxy") {
		cfg.Network.UpstreamProxy.NoProxy = nil
	}
	if !cfg.ManagedSets("network.upstream_proxy.resolve_via_proxy") {
		cfg.Network.UpstreamProxy.ResolveViaProxy = false
	}
}

// Upstream builds the egress upstream proxy of network.upstream_proxy, or nil.
func (c Config) Upstream() (*egress.Upstream, error) {
	p := c.Network.UpstreamProxy
	if p == nil {
		return nil, nil
	}
	u, err := proxyURL(p.URL)
	if err != nil {
		return nil, err
	}
	up := &egress.Upstream{URL: u, NoProxy: append([]string(nil), p.NoProxy...), ResolveViaProxy: p.ResolveViaProxy}
	if p.CAFile != "" {
		certs, err := readBundle(p.CAFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		for _, cert := range certs {
			pool.AddCert(cert)
		}
		up.Roots, up.Bundle = pool, certs
	}
	if name := p.CredentialsSecret; name != "" {
		// Read at each connection, so a rotated password is used at once.
		up.Credentials = func() (string, string, bool, error) {
			v, err := secrets.Default().Value(name)
			if err != nil {
				return "", "", false, fmt.Errorf("the secret %s: %w", name, err)
			}
			user, pass, ok := strings.Cut(v, ":")
			if !ok || user == "" {
				return "", "", false, fmt.Errorf("the secret %s is not user:password", name)
			}
			return user, pass, true, nil
		}
	}
	return up, nil
}

// ApplyNetwork puts the configuration's upstream proxy, or none, in force
// for the process's connections, where a session or command starts. The
// proxy is process-wide: a second, different one in the same process is
// refused (egress.ErrUpstreamConflict). Loading a configuration never does this.
func (c Config) ApplyNetwork() error {
	up, err := c.Upstream()
	if err != nil {
		return err
	}
	return egress.UseUpstream(up, c.networkKey())
}

// networkKey describes the upstream proxy settings, for telling two apart.
func (c Config) networkKey() string {
	p := c.Network.UpstreamProxy
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%s|%s|%s|%v|%s", p.URL, p.CAFile, p.CredentialsSecret, p.ResolveViaProxy, strings.Join(p.NoProxy, ","))
}
