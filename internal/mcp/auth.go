package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Authorizer gives the OAuth access token for one remote server's login.
type Authorizer interface {
	// Token is the access token, refreshed first when it is about to end.
	Token(ctx context.Context) (string, error)
	// Refresh replaces failed, which the server refused, with a new token.
	Refresh(ctx context.Context, failed string) (string, error)
}

// ErrBothAuth refuses a server given both a static Authorization header and
// a login, where which one is sent would be chance.
var ErrBothAuth = errors.New("it has both an Authorization header and an OAuth login; remove the header or run abhed mcp logout")

// authTransport adds the login's bearer token to requests for the server's
// own origin and nothing else: a redirect, or an endpoint the server names
// on another host, never carries it.
type authTransport struct {
	base   http.RoundTripper
	origin string
	auth   Authorizer
}

// requestOrigin is a request's scheme and host, default port left out.
func requestOrigin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" && (scheme != "https" || p != "443") && (scheme != "http" || p != "80") {
		host += ":" + p
	}
	return scheme + "://" + host
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if requestOrigin(req.URL) != t.origin {
		return t.base.RoundTrip(req)
	}
	tok, err := t.auth.Token(req.Context())
	if err != nil {
		return nil, fmt.Errorf("mcp: sign-in: %w", err)
	}
	resp, err := t.base.RoundTrip(withBearer(req, tok))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	// Refused: a token the server ended early is refreshed once, and the
	// request sent again when its body can be.
	if req.Body != nil && req.GetBody == nil {
		return resp, nil
	}
	next, rerr := t.auth.Refresh(req.Context(), tok)
	if rerr != nil || next == tok {
		return resp, nil //nolint:nilerr // the server's 401 is the answer, and it names why
	}
	retry := withBearer(req, next)
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return resp, nil //nolint:nilerr // the server's 401 stands
		}
		retry.Body = body
	}
	_ = resp.Body.Close()
	return t.base.RoundTrip(retry)
}

// withBearer is a copy of req with the token as its Authorization.
func withBearer(req *http.Request, tok string) *http.Request {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

// hasAuthorization reports a header map that sets Authorization, in any case.
func hasAuthorization(h map[string]string) bool {
	for k := range h {
		if strings.EqualFold(strings.TrimSpace(k), "authorization") {
			return true
		}
	}
	return false
}
