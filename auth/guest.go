package auth

import (
	"net/http"
	"sync"
	"time"
)

// Guests on a server's console (auth.guest, off by default): a person who
// is not signed in may work as a guest, an identity of its own made for that
// browser session and never reused. A guest is never more privileged than
// any account: no groups, the deployment's default tenant, and every rule,
// sandbox and approval as for anyone. Its sessions belong to guest:<id>, and
// when the guest signs out or its session expires, OnEnd is told so the
// server removes what it kept.

// ProviderGuest is the provider of a console guest's identity.
const ProviderGuest = "guest"

// GuestPath starts a guest's browser session.
const GuestPath = "/v1/guest"

// GuestAuth holds console guests' browser sessions.
type GuestAuth struct {
	TTL        time.Duration
	Secure     bool
	Tenant     string
	CookieName string
	// OnEnd is told each guest owner whose session ended, to remove what the
	// server kept for it. It runs on its own goroutine.
	OnEnd func(owner string)

	mu       sync.Mutex
	sessions map[string]*guestSession // by cookie digest
	now      func() time.Time
	stop     chan struct{}
	stopOnce sync.Once
}

type guestSession struct {
	subject string
	expires time.Time
}

// DefaultGuestTTL is how long a console guest's session lasts.
const DefaultGuestTTL = 8 * time.Hour

// maxGuests bounds the guest sessions one server holds, so an anonymous
// caller cannot grow the table without limit.
const maxGuests = 4096

// NewGuestAuth serves console guests.
func NewGuestAuth(ttl time.Duration, secure bool, tenant string, onEnd func(owner string)) *GuestAuth {
	if ttl <= 0 {
		ttl = DefaultGuestTTL
	}
	g := &GuestAuth{TTL: ttl, Secure: secure, Tenant: tenant, CookieName: "abhed_guest", OnEnd: onEnd,
		sessions: map[string]*guestSession{}, now: time.Now, stop: make(chan struct{})}
	go g.reapLoop()
	return g
}

// Close stops the reaper.
func (g *GuestAuth) Close() { g.stopOnce.Do(func() { close(g.stop) }) }

func (g *GuestAuth) reapLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-t.C:
			g.reap()
		}
	}
}

// reap ends every expired guest session.
func (g *GuestAuth) reap() {
	now := g.now()
	var ended []string
	g.mu.Lock()
	for k, s := range g.sessions {
		if !now.Before(s.expires) {
			delete(g.sessions, k)
			ended = append(ended, GuestOwner(s.subject))
		}
	}
	g.mu.Unlock()
	g.ended(ended...)
}

func (g *GuestAuth) ended(owners ...string) {
	if g.OnEnd == nil {
		return
	}
	for _, o := range owners {
		go g.OnEnd(o)
	}
}

// GuestOwner is the owner of a console guest's sessions.
func GuestOwner(subject string) string { return ProviderGuest + ":" + subject }

// Name is the provider's name.
func (g *GuestAuth) Name() string { return ProviderGuest }

// Identify resolves the guest cookie.
func (g *GuestAuth) Identify(r *http.Request) (*Identity, bool) {
	c, err := r.Cookie(g.CookieName)
	if err != nil || c.Value == "" {
		return nil, false
	}
	key := sessionDigest(c.Value)
	g.mu.Lock()
	s, ok := g.sessions[key]
	if ok && !g.now().Before(s.expires) {
		delete(g.sessions, key)
		ok = false
		defer g.ended(GuestOwner(s.subject))
	}
	g.mu.Unlock()
	if !ok {
		return nil, false
	}
	return &Identity{Subject: s.subject, Name: "Guest", Tenant: g.Tenant, Provider: ProviderGuest,
		Expires: s.expires.Unix()}, true
}

// Routes serves POST /v1/guest.
func (g *GuestAuth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+GuestPath, g.start)
}

// PublicPaths is the guest start, reachable before any sign-in.
func (g *GuestAuth) PublicPaths() []string { return []string{GuestPath} }

// SignIn offers no redirect: the console shows its own button.
func (g *GuestAuth) SignIn() (url, label string) { return "", "" }

func (g *GuestAuth) start(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	sid := randomToken()
	subject := randomToken()[:16]
	now := g.now()
	g.mu.Lock()
	if len(g.sessions) >= maxGuests {
		g.mu.Unlock()
		writeAuthJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "too many guests; try again later"})
		return
	}
	g.sessions[sessionDigest(sid)] = &guestSession{subject: subject, expires: now.Add(g.TTL)}
	g.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: g.CookieName, Value: sid, Path: "/", HttpOnly: true, Secure: g.Secure,
		SameSite: http.SameSiteStrictMode, Expires: now.Add(g.TTL)})
	writeAuthJSON(w, http.StatusOK, map[string]any{"guest": true, "owner": GuestOwner(subject), "expires": now.Add(g.TTL).Unix()})
}

// SignOut ends the guest's session; what it kept is removed.
func (g *GuestAuth) SignOut(w http.ResponseWriter, r *http.Request) {
	g.EndRequestSession(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// EndRequestSession ends the request's guest session and expires its cookie.
func (g *GuestAuth) EndRequestSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(g.CookieName); err == nil {
		key := sessionDigest(c.Value)
		g.mu.Lock()
		s, ok := g.sessions[key]
		delete(g.sessions, key)
		g.mu.Unlock()
		if ok {
			g.ended(GuestOwner(s.subject))
		}
	}
	http.SetCookie(w, &http.Cookie{Name: g.CookieName, Value: "", Path: "/", HttpOnly: true, Secure: g.Secure,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
}
