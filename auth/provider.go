package auth

import (
	"context"
	"net/http"
)

// Provider is one way a person can hold a browser session.
//
// The middleware used to know its providers by name: a concrete field for
// local accounts and another for the OIDC login. That worked while both lived
// in one package and stopped working the moment they did not — an edition
// without OIDC could not compile the middleware, and one with a different
// identity mechanism had nowhere to put it. The interface is the cut: the
// middleware asks each provider whether it recognises the request and never
// learns what a cookie or a token looks like.
//
// A provider owns its endpoints. It registers them itself, says which of
// them must answer before anyone is signed in, and names its entry point so
// the middleware can send a browser there. The server registers nothing on a
// provider's behalf; that is what lets a provider move between editions
// without the server changing.
type Provider interface {
	// Name is the short mode name the console reports: "local", "oidc".
	Name() string
	// Identify resolves a browser session this provider holds, if any.
	Identify(r *http.Request) (*Identity, bool)
	// Routes registers the provider's own endpoints.
	Routes(mux *http.ServeMux)
	// PublicPaths are the paths that must answer without a session — the
	// sign-in endpoints themselves, or the only way in is barred by the thing
	// it unlocks.
	PublicPaths() []string
	// SignIn is the browser entry point and the name to put on its button.
	// Both empty when the provider has no page of its own: local accounts
	// render their form on the front door, which the server owns.
	SignIn() (url, label string)
	// SignOut ends the session this provider holds and sends the browser on.
	SignOut(w http.ResponseWriter, r *http.Request)
}

// SessionEnder is implemented by a provider that can end the session a
// request carries without answering it, so a refused session can be cleared.
// A provider without it is signed out through SignOut, its response discarded
// except for the cookies it sets.
type SessionEnder interface {
	EndRequestSession(w http.ResponseWriter, r *http.Request)
}

// TokenVerifier validates a bearer token for an API client. Separate from
// Provider because a token has no session to hold: it is verified on every
// request, by whatever issued it, and the middleware needs only the verdict.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (*Identity, error)
}

// Checker is implemented by a provider that can prove its configuration works
// before a request depends on it — reaching the identity provider's keys, say.
// Optional: `abhed doctor` runs it where present and says nothing otherwise,
// so the doctor stays the same command in every edition.
type Checker interface {
	Check(ctx context.Context) error
}
