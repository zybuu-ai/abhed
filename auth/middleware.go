package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

type ctxKey string

const identityKey ctxKey = "abhed.identity"

// FromContext returns the verified identity, if any.
func FromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(identityKey).(*Identity)
	return id, ok
}

// WithIdentity is exported for tests and for transports that authenticate
// outside HTTP.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// Middleware establishes the caller's identity on every request.
//
// Three modes, because deployments genuinely differ:
//
//   - providers or a verifier → a session or a token is required
//   - neither, TrustHeaders    → identity comes from proxy headers
//   - neither                  → anonymous single-tenant, for local dev
//
// The proxy-header path is only safe when a trusted proxy is the sole route to
// the port; it is not the default, and `abhed doctor` says which mode is live.
type Middleware struct {
	// Providers hold browser sessions, in the order they are asked. The first
	// with an entry point is where a browser without a session is sent.
	Providers []Provider
	// Verifier validates bearer tokens for API clients. A session still takes
	// precedence, so the console works without every request carrying a token.
	Verifier     TokenVerifier
	TrustHeaders bool
	// PublicPaths bypass authentication (health checks, the console shell).
	PublicPaths []string
	// Check, when set, runs after a provider, the verifier or a trusted proxy
	// has identified someone. An error refuses the request, ends the session a
	// provider holds, and its text is shown to the person, so keep it plain.
	// A stream runs it again through Recheck for as long as it stays open.
	Check func(ctx context.Context, id *Identity) error
}

func (m Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public paths skip verification but still receive an anonymous
		// identity: a handler must never have to nil-check what the
		// middleware is responsible for providing.
		for _, p := range m.PublicPaths {
			if r.URL.Path == p {
				next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(),
					&Identity{Subject: "anonymous", Tenant: "default"})))
				return
			}
		}

		// An identity already in the context was established by whoever
		// dispatched here — an outer router that authenticated once and then
		// chose this server. Asking again would either fail, because that
		// router holds the session and this middleware does not, or succeed
		// and disagree. Either way the outer answer stands.
		if _, ok := FromContext(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}

		id, p, fail, err := m.authenticate(r)
		switch fail {
		case failChallenge:
			m.challenge(w, r, err.Error())
			return
		case failToken:
			// The reason is safe to return: it helps a legitimate client
			// fix its configuration and tells an attacker nothing they
			// could not determine by trying.
			unauthorized(w, err.Error())
			return
		case failRefused:
			if p != nil {
				endSession(w, r, p)
				noteRefusal(w, r, p, err)
			}
			m.refuse(w, r, err)
			return
		}
		if p != nil {
			forgetRefusal(w, r, p)
		}
		// The request as it arrived, so a stream can be authenticated again
		// by exactly this path for as long as it stays open.
		orig := r
		ctx := WithRecheck(WithIdentity(r.Context(), id), func(ctx context.Context) (*Identity, error) {
			id, _, fail, err := m.authenticate(orig.Clone(ctx))
			if fail != failNone {
				return nil, err
			}
			return id, nil
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authFailure says how a request that could not be authenticated is answered.
type authFailure int

const (
	failNone      authFailure = iota
	failChallenge             // no credentials: sign in
	failToken                 // a bearer token that did not verify
	failRefused               // identified, then turned away by Check
)

// authenticate identifies the caller the way every request is identified. It
// writes nothing, so Wrap and a stream's recheck share one definition. p is
// the provider that recognised the request, if one did.
func (m Middleware) authenticate(r *http.Request) (id *Identity, p Provider, fail authFailure, err error) {
	for _, p := range m.Providers {
		if id, ok := p.Identify(r); ok {
			if err := m.check(r.Context(), id); err != nil {
				return nil, p, failRefused, err
			}
			return id, p, failNone, nil
		}
	}

	if m.Verifier != nil {
		token := bearerToken(r)
		if token == "" {
			return nil, nil, failChallenge, errors.New("missing bearer token")
		}
		id, err := m.Verifier.Verify(r.Context(), token)
		if err != nil {
			return nil, nil, failToken, err
		}
		if err := m.check(r.Context(), id); err != nil {
			return nil, nil, failRefused, err
		}
		return id, nil, failNone, nil
	}

	if len(m.Providers) > 0 {
		// Sessions are the whole authentication mechanism here, not a
		// fallback: no session means not signed in.
		return nil, nil, failChallenge, errors.New("sign in required")
	}

	if m.TrustHeaders {
		id := headerIdentity(r)
		if proxied(r) {
			if err := m.check(r.Context(), id); err != nil {
				return nil, nil, failRefused, err
			}
		}
		return id, nil, failNone, nil
	}

	return &Identity{Subject: "anonymous", Tenant: "default"}, nil, failNone, nil
}

const recheckKey ctxKey = "abhed.recheck"

// WithRecheck attaches how a request's authentication is run again. Wrap sets
// it; an outer router that authenticates on this server's behalf sets its own,
// or its streams are trusted for as long as they stay open.
func WithRecheck(ctx context.Context, fn func(context.Context) (*Identity, error)) context.Context {
	return context.WithValue(ctx, recheckKey, fn)
}

// Recheck authenticates the request behind ctx again, as a new request with
// the same credentials would be now: a sign-out, a removed account or a Check
// that now refuses all fail it. A long-lived response calls it while it runs,
// since the middleware saw it only once. With nothing to rerun it returns the
// identity already in ctx.
func Recheck(ctx context.Context) (*Identity, error) {
	if fn, ok := ctx.Value(recheckKey).(func(context.Context) (*Identity, error)); ok && fn != nil {
		return fn(ctx)
	}
	id, _ := FromContext(ctx)
	return id, nil
}

// headerIdentity is the identity a trusted proxy asserted, anonymous and
// without groups when it named no user.
func headerIdentity(r *http.Request) *Identity {
	id := &Identity{
		Subject: headerOr(r, "X-Abhed-User", "anonymous"),
		Email:   r.Header.Get("X-Abhed-Email"),
		Tenant:  headerOr(r, "X-Abhed-Tenant", "default"),
	}
	if proxied(r) {
		// The proxy is the only way in and vouches for both headers.
		id.Provider, id.EmailVerified = ProviderProxy, true
	}
	if groups := r.Header.Get("X-Abhed-Groups"); groups != "" && r.Header.Get("X-Abhed-User") != "" {
		id.Groups = strings.Split(groups, ",")
	}
	return id
}

// proxied reports whether a trusted proxy named the caller, by user or email.
func proxied(r *http.Request) bool {
	return r.Header.Get("X-Abhed-User") != "" || r.Header.Get("X-Abhed-Email") != ""
}

// ProxyMode reports whether identity comes from a trusted proxy's headers:
// no provider holds sessions and no verifier checks tokens.
func (m Middleware) ProxyMode() bool {
	return m.TrustHeaders && len(m.Providers) == 0 && m.Verifier == nil
}

// Identify reports who a request's session or proxy headers name, applying
// Check, for handlers on public paths that still say who is signed in. mode
// is the provider's name, or "proxy". A refused session is ended and its
// reason returned; nil, "", nil means nobody is signed in.
func (m Middleware) Identify(w http.ResponseWriter, r *http.Request) (id *Identity, mode string, err error) {
	for _, p := range m.Providers {
		if id, ok := p.Identify(r); ok {
			if err := m.check(r.Context(), id); err != nil {
				endSession(w, r, p)
				noteRefusal(w, r, p, err)
				return nil, p.Name(), err
			}
			return id, p.Name(), nil
		}
	}
	if m.ProxyMode() && proxied(r) {
		id := headerIdentity(r)
		if err := m.check(r.Context(), id); err != nil {
			return nil, "proxy", err
		}
		return id, "proxy", nil
	}
	return nil, "", nil
}

func (m Middleware) check(ctx context.Context, id *Identity) error {
	if m.Check == nil {
		return nil
	}
	return m.Check(ctx, id)
}

// refuse answers a request whose identity Check turned away: a browser goes
// to the front door with the reason, an API client gets 403 and the reason.
func (m Middleware) refuse(w http.ResponseWriter, r *http.Request, err error) {
	if wantsHTML(r) {
		http.Redirect(w, r, "/?refused="+url.QueryEscape(clip(err.Error(), 200)), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": "forbidden", "reason": err.Error(), "refused": true})
}

// refusedCookie remembers for a while why a session was ended, so the sign-in
// page a reload lands on can still say it.
const refusedCookie = "abhed_refused"

func noteRefusal(w http.ResponseWriter, r *http.Request, p Provider, err error) {
	// #nosec G124 -- Secure follows the provider's session cookie; HttpOnly and SameSite are set.
	http.SetCookie(w, &http.Cookie{Name: refusedCookie, Value: url.QueryEscape(clip(err.Error(), 200)),
		Path: "/", MaxAge: 600, HttpOnly: true, Secure: cookieSecure(r, p), SameSite: http.SameSiteLaxMode})
}

func forgetRefusal(w http.ResponseWriter, r *http.Request, p Provider) {
	if _, err := r.Cookie(refusedCookie); err == nil {
		// #nosec G124 -- Secure follows the provider's session cookie; HttpOnly and SameSite are set.
		http.SetCookie(w, &http.Cookie{Name: refusedCookie, Path: "/", MaxAge: -1, HttpOnly: true,
			Secure: cookieSecure(r, p), SameSite: http.SameSiteLaxMode})
	}
}

// cookieSecure follows the provider's own session cookie where it says, so a
// TLS-terminating proxy in front still gets a Secure cookie.
func cookieSecure(r *http.Request, p Provider) bool {
	if c, ok := p.(interface{ CookieSecure() bool }); ok {
		return c.CookieSecure()
	}
	return r.TLS != nil
}

// clip shortens s to at most n runes.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// endSession ends the session p holds for r. SignOut is the fallback, with
// everything but its cookies discarded, since it also answers the request.
func endSession(w http.ResponseWriter, r *http.Request, p Provider) {
	if e, ok := p.(SessionEnder); ok {
		e.EndRequestSession(w, r)
		return
	}
	scratch := &headerOnly{h: http.Header{}}
	p.SignOut(scratch, r)
	for _, c := range scratch.h.Values("Set-Cookie") {
		w.Header().Add("Set-Cookie", c)
	}
}

// headerOnly keeps the headers written to it and drops everything else.
type headerOnly struct{ h http.Header }

func (h *headerOnly) Header() http.Header         { return h.h }
func (h *headerOnly) Write(b []byte) (int, error) { return len(b), nil }
func (h *headerOnly) WriteHeader(int)             {}

// challenge answers a request with no credentials. A browser is sent to sign
// in, keeping where it was going; an API client gets 401 and a reason.
//
// The destination is the first provider with an entry point of its own. With
// none — local accounts only — it is the front door, where the server renders
// the sign-in form. Either way the page receives ?return= so it can send the
// person back afterwards.
func (m Middleware) challenge(w http.ResponseWriter, r *http.Request, reason string) {
	if !wantsHTML(r) || len(m.Providers) == 0 {
		unauthorized(w, reason)
		return
	}
	target := "/"
	for _, p := range m.Providers {
		if u, _ := p.SignIn(); u != "" {
			target = u
			break
		}
	}
	q := "?return=" + url.QueryEscape(r.URL.RequestURI())
	if c, err := r.Cookie(refusedCookie); err == nil {
		if why, err := url.QueryUnescape(c.Value); err == nil && why != "" {
			q = "?refused=" + url.QueryEscape(clip(why, 200)) + "&return=" + url.QueryEscape(r.URL.RequestURI())
		}
	}
	http.Redirect(w, r, target+q, http.StatusFound)
}

// wantsHTML distinguishes a browser navigation from an API call, so only the
// former is redirected to a sign-in page.
func wantsHTML(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		strings.Contains(r.Header.Get("Accept"), "text/html")
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func headerOr(r *http.Request, key, fallback string) string {
	if v := r.Header.Get(key); v != "" {
		return v
	}
	return fallback
}

func unauthorized(w http.ResponseWriter, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="abhed"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized", "reason": reason})
}

// RequireGroup gates a handler on group membership, for RBAC above tenancy.
func RequireGroup(group string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := FromContext(r.Context())
		if !ok {
			unauthorized(w, "no identity")
			return
		}
		for _, g := range id.Groups {
			if g == group {
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "forbidden", "reason": "requires group " + group})
	})
}
