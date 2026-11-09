// Package mcpauth signs Abhed in to a remote MCP server with OAuth 2.1, as
// the MCP authorization specification describes: protected-resource and
// authorization-server metadata, PKCE (S256 only), dynamic client
// registration where the server offers it, and a loopback redirect.
//
// The tokens are kept in Abhed's secret store under names the store holds
// for Abhed itself (secrets.HeldPrefix): never offered to the model, never
// given to a command, redacted from the record like any stored value. Each
// login is bound to the server's name and canonical URL; the access token is
// sent to that origin only, and the refresh token to the token endpoint the
// login found, never to an address learned later.
package mcpauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// maxBody bounds every metadata, registration and token answer.
const maxBody = 1 << 20

// Metadata is what discovery found for one MCP server.
type Metadata struct {
	// Resource is the server's canonical URL, sent as the resource parameter.
	Resource string
	Issuer   string
	// AuthorizationEndpoint is opened in the person's browser; the others are
	// reached by Abhed.
	AuthorizationEndpoint string
	TokenEndpoint         string
	RegistrationEndpoint  string
	RevocationEndpoint    string
	// Scopes are the scopes the resource says it supports, asked for when
	// the configuration names none.
	Scopes []string
	// ISSResponse is set when the authorization server adds iss to its
	// redirect (RFC 9207), which is then checked.
	ISSResponse bool
	// AuthMethods are the token endpoint's client authentication methods.
	AuthMethods []string
}

// Canonical is a server URL as a login is bound to it and sent as the
// resource: scheme and host in lower case, no default port, no fragment, no
// trailing slash, no user information.
func Canonical(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "", fmt.Errorf("%q is not an absolute http or https URL", printable(raw))
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%q is not an http or https URL", printable(raw))
	}
	if u.User != nil {
		return "", errors.New("a server URL with user information cannot be signed in to")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "443" && u.Scheme == "https" || port == "80" && u.Scheme == "http" {
		port = ""
	}
	u.Host = host
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	u.Fragment, u.RawFragment = "", ""
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// origin is a URL's scheme and host, with its default port left out.
func origin(raw string) string {
	c, err := Canonical(raw)
	if err != nil {
		return ""
	}
	u, _ := url.Parse(c)
	return u.Scheme + "://" + u.Host
}

// Origin is a URL's scheme and host as Canonical writes them; "" for one
// that is not an http or https URL.
func Origin(raw string) string { return origin(raw) }

// loopback reports a host that is this machine.
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// endpointURL checks an endpoint the authorization server names: absolute,
// https (http only on this machine), no user information or fragment.
func endpointURL(what, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("the %s %q is not an absolute URL", what, printable(raw))
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && loopback(u.Hostname()):
	default:
		return "", fmt.Errorf("the %s %q is not https", what, printable(raw))
	}
	return u.String(), nil
}

// get fetches a JSON document; status 0 with an error is a failure to reach.
func get(ctx context.Context, hc *http.Client, raw string, into any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return resp.StatusCode, err
	}
	if len(body) > maxBody {
		return resp.StatusCode, fmt.Errorf("%s answered more than %d bytes", printable(raw), maxBody)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return resp.StatusCode, fmt.Errorf("%s is not JSON metadata: %w", printable(raw), err)
	}
	return resp.StatusCode, nil
}

// wellKnown inserts a well-known suffix between a URL's host and path
// (RFC 8414 §3.1, RFC 9728 §3.1).
func wellKnown(raw, suffix string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	path := strings.TrimRight(u.Path, "/")
	return u.Scheme + "://" + u.Host + "/.well-known/" + suffix + path
}

// challenge asks the server for its protected-resource metadata URL and
// scope: the WWW-Authenticate of its 401 to an unauthenticated initialize.
func challenge(ctx context.Context, hc *http.Client, server string) (metaURL, scope string) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"abhed","version":"0.1.0"}}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server, bytes.NewReader(body))
	if err != nil {
		return "", ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := hc.Do(req)
	if err != nil {
		return "", ""
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return "", ""
	}
	for _, h := range resp.Header.Values("WWW-Authenticate") {
		params := bearerParams(h)
		if params == nil {
			continue
		}
		return params["resource_metadata"], params["scope"]
	}
	return "", ""
}

// bearerParams reads a Bearer challenge's parameters, nil for another scheme.
func bearerParams(h string) map[string]string {
	h = strings.TrimSpace(h)
	if len(h) < 6 || !strings.EqualFold(h[:6], "bearer") {
		return nil
	}
	out := map[string]string{}
	rest := h[6:]
	for len(rest) > 0 {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimLeft(v, " ")
		if strings.HasPrefix(v, `"`) {
			end := strings.IndexByte(v[1:], '"')
			if end < 0 {
				break
			}
			out[k], rest = v[1:end+1], v[end+2:]
			continue
		}
		val, after, _ := strings.Cut(v, ",")
		out[k], rest = strings.TrimSpace(val), after
	}
	return out
}

// resourceCovers reports whether the resource a metadata document names is
// the server, or a path above it on the same origin.
func resourceCovers(resource, server string) bool {
	r, err := Canonical(resource)
	if err != nil {
		return false
	}
	return r == server || strings.HasPrefix(server, r+"/")
}

// Discover finds how to sign in to the MCP server at serverURL. Every
// request goes through hc, which the caller gives the egress guard and the
// upstream proxy, and which must not follow redirects.
func Discover(ctx context.Context, hc *http.Client, serverURL string) (Metadata, error) {
	server, err := Canonical(serverURL)
	if err != nil {
		return Metadata{}, err
	}
	md := Metadata{Resource: server}
	// A bearer token is never sent in the clear.
	if _, err := endpointURL("MCP server URL", server); err != nil {
		return md, err
	}
	metaURL, scope := challenge(ctx, hc, server)
	var candidates []string
	if metaURL != "" {
		u, err := endpointURL("protected-resource metadata URL", metaURL)
		if err != nil {
			return md, err
		}
		candidates = append(candidates, u)
	}
	candidates = append(candidates, wellKnown(server, "oauth-protected-resource"))
	if root := origin(server) + "/.well-known/oauth-protected-resource"; !slices.Contains(candidates, root) {
		candidates = append(candidates, root)
	}
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	found := false
	for _, c := range candidates {
		status, err := get(ctx, hc, c, &prm)
		if err != nil && status == 0 {
			return md, fmt.Errorf("protected-resource metadata: %w", err)
		}
		if err != nil {
			return md, err
		}
		if status == http.StatusOK {
			found = true
			break
		}
	}
	// A server with no resource metadata is its own authorization server.
	issuer := origin(server)
	if found {
		// The resource must be the server it was asked of, so a document
		// served for another resource cannot send this login elsewhere.
		if !resourceCovers(prm.Resource, server) {
			return md, fmt.Errorf("the protected-resource metadata names the resource %q, not %s", printable(prm.Resource), server)
		}
		if len(prm.AuthorizationServers) == 0 {
			return md, errors.New("the protected-resource metadata names no authorization server")
		}
		if issuer, err = endpointURL("authorization server", prm.AuthorizationServers[0]); err != nil {
			return md, err
		}
		md.Scopes = prm.ScopesSupported
	}
	if scope != "" {
		md.Scopes = strings.Fields(scope)
	}
	if err := authServer(ctx, hc, issuer, &md); err != nil {
		return md, err
	}
	return md, nil
}

// authServer reads the authorization server's metadata into md, checking
// that it is the issuer asked for and that it does PKCE with S256.
func authServer(ctx context.Context, hc *http.Client, issuer string, md *Metadata) error {
	var meta struct {
		Issuer                string   `json:"issuer"`
		AuthorizationEndpoint string   `json:"authorization_endpoint"`
		TokenEndpoint         string   `json:"token_endpoint"`
		RegistrationEndpoint  string   `json:"registration_endpoint"`
		RevocationEndpoint    string   `json:"revocation_endpoint"`
		CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
		ResponseTypes         []string `json:"response_types_supported"`
		AuthMethods           []string `json:"token_endpoint_auth_methods_supported"`
		ISSParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
	}
	u, _ := url.Parse(issuer)
	path := strings.TrimRight(u.Path, "/")
	candidates := []string{wellKnown(issuer, "oauth-authorization-server"), wellKnown(issuer, "openid-configuration")}
	if path != "" {
		candidates = append(candidates, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration")
	}
	found := false
	for _, c := range candidates {
		status, err := get(ctx, hc, c, &meta)
		if err != nil {
			return fmt.Errorf("authorization server metadata: %w", err)
		}
		if status == http.StatusOK {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("the authorization server %s publishes no metadata", printable(issuer))
	}
	// RFC 8414 §3.3: the document must be the issuer's own.
	if strings.TrimRight(meta.Issuer, "/") != strings.TrimRight(issuer, "/") {
		return fmt.Errorf("the authorization server metadata names the issuer %q, not %s", printable(meta.Issuer), printable(issuer))
	}
	if !slices.Contains(meta.CodeChallengeMethods, "S256") {
		return fmt.Errorf("the authorization server %s does not offer PKCE with S256, which a sign-in requires", printable(issuer))
	}
	if len(meta.ResponseTypes) > 0 && !slices.Contains(meta.ResponseTypes, "code") {
		return fmt.Errorf("the authorization server %s does not offer the authorization code flow", printable(issuer))
	}
	var err error
	md.Issuer = meta.Issuer
	if md.AuthorizationEndpoint, err = endpointURL("authorization endpoint", meta.AuthorizationEndpoint); err != nil {
		return err
	}
	if md.TokenEndpoint, err = endpointURL("token endpoint", meta.TokenEndpoint); err != nil {
		return err
	}
	if meta.RegistrationEndpoint != "" {
		if md.RegistrationEndpoint, err = endpointURL("registration endpoint", meta.RegistrationEndpoint); err != nil {
			return err
		}
	}
	if meta.RevocationEndpoint != "" {
		// Revocation is best effort: one that is not https is left unused.
		md.RevocationEndpoint, _ = endpointURL("revocation endpoint", meta.RevocationEndpoint)
	}
	md.ISSResponse = meta.ISSParameterSupported
	md.AuthMethods = meta.AuthMethods
	return nil
}

// printable is untrusted text as an error may show it: control characters
// escaped and the length bounded.
func printable(s string) string {
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	q := fmt.Sprintf("%+q", s)
	return q[1 : len(q)-1]
}
