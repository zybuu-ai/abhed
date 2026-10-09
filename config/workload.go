package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// WorkloadAuthConfig is auth.workload: workloads that prove a SPIFFE
// identity, and short-lived tokens exchanged for narrower ones (RFC 8693).
// The Enterprise edition serves it; the Community Edition checks these keys
// and refuses enabled.
type WorkloadAuthConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Audience is what a JWT-SVID must name in aud to be accepted here, and
	// what every token this server exchanges carries.
	Audience string `json:"audience,omitempty"`
	// TrustDomain is the one SPIFFE trust domain accepted, e.g. "example.org".
	TrustDomain string `json:"trust_domain,omitempty"`
	// JWTBundleFile is the trust domain's bundle as a JWKS (SPIFFE bundle
	// format); it verifies JWT-SVIDs and may carry X.509 roots too.
	JWTBundleFile string `json:"jwt_bundle_file,omitempty"`
	// X509BundleFile holds the trust domain's X.509 roots in PEM, for
	// X.509-SVIDs a client presents over TLS.
	X509BundleFile string `json:"x509_bundle_file,omitempty"`
	// ReloadSeconds is how often the bundle files are checked; 30 when unset.
	ReloadSeconds int `json:"reload_seconds,omitempty"`
	// KeyFile holds the key exchanged tokens are signed and sealed with;
	// ~/.abhed/workload-token.key when unset.
	KeyFile string `json:"key_file,omitempty"`
	// TokenMinutes caps an exchanged token's life; 10 when unset, at most 60.
	TokenMinutes int `json:"token_minutes,omitempty"`
	// Grants say what each workload may ask for. A workload no grant names
	// is refused.
	Grants []WorkloadGrant `json:"grants,omitempty"`
}

// WorkloadGrant is the most one workload, or a family of them, may receive.
type WorkloadGrant struct {
	// SPIFFEID is one workload, spiffe://domain/path, or every workload under
	// a path when it ends in "/*".
	SPIFFEID string `json:"spiffe_id"`
	// Scopes are the most a token for it may carry.
	Scopes []string `json:"scopes"`
	// Tenant is the tenant it acts in; auth.default_tenant when unset.
	Tenant string `json:"tenant,omitempty"`
	// AllowReusableSVID accepts its JWT-SVIDs without a jti, which can then
	// be exchanged more than once until they expire. Off: a JWT-SVID needs a
	// jti and is exchanged once.
	AllowReusableSVID bool `json:"allow_reusable_svid,omitempty"`
}

var trustDomainRe = regexp.MustCompile(`^[a-z0-9._-]{1,255}$`)

func (w WorkloadAuthConfig) validate() error {
	if w.TokenMinutes < 0 || w.TokenMinutes > 60 {
		return fmt.Errorf("auth.workload.token_minutes is %d; use 1 to 60, or 0 for 10", w.TokenMinutes)
	}
	if w.ReloadSeconds < 0 || w.ReloadSeconds > 3600 {
		return fmt.Errorf("auth.workload.reload_seconds is %d; use 1 to 3600, or 0 for 30", w.ReloadSeconds)
	}
	if !w.Enabled {
		return nil
	}
	if !trustDomainRe.MatchString(w.TrustDomain) {
		return fmt.Errorf("auth.workload.trust_domain %q is not a trust domain name (lower-case letters, digits, '.', '-', '_')", w.TrustDomain)
	}
	if strings.TrimSpace(w.Audience) == "" {
		return fmt.Errorf("auth.workload.audience is required: a JWT-SVID minted for another service must not be accepted here")
	}
	if w.JWTBundleFile == "" && w.X509BundleFile == "" {
		return fmt.Errorf("auth.workload needs jwt_bundle_file or x509_bundle_file, the trust domain's bundle")
	}
	if len(w.Grants) == 0 {
		return fmt.Errorf("auth.workload.grants is empty, so no workload could be given a token")
	}
	prefix := "spiffe://" + w.TrustDomain + "/"
	for _, g := range w.Grants {
		id := strings.TrimSuffix(g.SPIFFEID, "/*")
		if !strings.HasPrefix(id, prefix) || len(id) == len(prefix) {
			return fmt.Errorf("auth.workload.grants: %q is not a workload in trust domain %s", g.SPIFFEID, w.TrustDomain)
		}
		if strings.Contains(id, "*") {
			return fmt.Errorf("auth.workload.grants: %q may end in /* and have no other wildcard", g.SPIFFEID)
		}
		if len(g.Scopes) == 0 {
			return fmt.Errorf("auth.workload.grants: %s names no scopes", g.SPIFFEID)
		}
	}
	return nil
}

// WorkloadKeyFile is where exchanged tokens' key is kept: auth.workload.key_file,
// else ~/.abhed/workload-token.key.
func (c Config) WorkloadKeyFile(workspace string) (string, error) {
	if p := c.Auth.Workload.KeyFile; p != "" {
		return serverPath(workspace, p), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".abhed", "workload-token.key"), nil
}

// ResolvedWorkload is auth.workload with its paths made absolute.
func (c Config) ResolvedWorkload(workspace string) WorkloadAuthConfig {
	w := c.Auth.Workload
	w.JWTBundleFile = serverPath(workspace, w.JWTBundleFile)
	w.X509BundleFile = serverPath(workspace, w.X509BundleFile)
	w.KeyFile = serverPath(workspace, w.KeyFile)
	return w
}
