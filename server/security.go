package server

import (
	"crypto/rand"
	"encoding/base32"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Controls that only matter once the server is reachable from a network it does
// not control.
//
// Everything here assumes the hostile case: the caller is anonymous, malicious,
// and automated. On a laptop behind a firewall these are redundant; on a public
// domain they are the difference between a login page and an open shell. They
// live in one file so an operator reviewing "what protects this thing" has a
// single place to read rather than a diff spread across the request path.

// requestMode resolves the permission mode for a session, given what the
// client asked for and what the operator configured.
//
// A client may only ever NARROW its own permissions. The obvious reading —
// "honour the mode in the request" — hands any caller a permission-escalation
// primitive: POST {"mode":"bypass"} and the approval gate is gone, along with
// every ask rule the operator wrote. The mode is attacker-controlled input, so
// it is treated as a request rather than an instruction.
//
// An unknown mode is rejected rather than ignored. Silently falling back to the
// configured mode would let a typo ("paln") read as success while running with
// permissions the caller did not intend.
func requestMode(configured, requested string) (string, bool) {
	if requested == "" {
		return configured, true
	}
	// plan is read-only and strictly narrower than anything an operator would
	// configure, so it is the one mode a client may select for itself. The
	// others (auto, accept-edits, bypass) all widen what runs without asking.
	if requested == "plan" {
		return requested, true
	}
	// Asking for exactly what is already configured is a no-op, not an
	// escalation, and clients that echo the mode back should not break.
	if requested == configured {
		return configured, true
	}
	return "", false
}

// newSessionID returns an unguessable session identifier.
//
// The previous scheme was a nanosecond timestamp in base36, which is monotonic
// and therefore enumerable: knowing one ID gives an attacker a small search
// space around it, and session lookup is scoped by tenant rather than by user.
// Randomness is what makes an ID unguessable, so the ID carries 120 bits of it.
func newSessionID() string {
	var b [15]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; if it ever does, a predictable
		// ID is not an acceptable fallback.
		panic("server: crypto/rand unavailable: " + err.Error())
	}
	return "s-" + strings.ToLower(base32.StdEncoding.WithPadding(
		base32.NoPadding).EncodeToString(b[:]))
}

// safeSegment matches an identifier that is safe to use as one path element.
var safeSegment = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// validSessionID reports whether id can be used as a path segment on disk.
//
// The upload handler joins the session ID from the URL into a filesystem path.
// filepath.Join cleans "..", which sounds like protection but is the opposite:
// it resolves the traversal instead of rejecting it, so the write lands
// somewhere real, just not where the handler intended. Validating the segment
// first means the traversal never reaches Join.
func validSessionID(id string) bool { return safeSegment.MatchString(id) }

// securityHeaders sets the response headers that constrain what a browser will
// do with our pages, on every response including errors and JSON.
//
// These are cheap, static, and only meaningful in aggregate: each one closes a
// class of attack that the others do not. They are set here rather than at the
// reverse proxy so the guarantee travels with the binary — a deployment that
// forgets a proxy directive is still covered.
func securityHeaders(next http.Handler, hsts bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Never let a browser second-guess our Content-Type. JSON that gets
		// sniffed as HTML is a stored-XSS vector.
		h.Set("X-Content-Type-Options", "nosniff")
		// The console drives an agent that runs commands; framing it is
		// clickjacking with unusually high stakes. frame-ancestors in the CSP
		// covers modern browsers, this covers the rest.
		h.Set("X-Frame-Options", "DENY")
		// Session IDs appear in URLs (/v1/sessions/{id}/events), so a referrer
		// leak is a capability leak.
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		// Nothing here needs hardware. Denying it costs nothing and removes the
		// permission prompts entirely.
		h.Set("Permissions-Policy",
			"camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		// HSTS is only set when TLS is actually terminated in front of us:
		// sending it over plain HTTP on a laptop would pin localhost to HTTPS
		// in the developer's browser and be a nuisance to undo.
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin rejects state-changing requests that a browser was tricked into
// sending from another site.
//
// SameSite=Lax on the session cookie already blocks the classic cross-site POST,
// but it is one cookie-flag change away from not doing so, and it offers nothing
// at all in proxy-header mode where identity does not come from a cookie. An
// Origin check is the direct statement of the rule, so it does not depend on a
// cookie attribute set somewhere else.
//
// Requests with no Origin header are allowed: non-browser clients (curl, the
// SDK, CI) legitimately omit it, and they are not the threat this addresses —
// a CSRF attack requires a browser, and browsers send Origin on exactly the
// requests that matter.
func sameOrigin(allowed []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			origin := r.Header.Get("Origin")
			// Under no-referrer Chrome sends a same-origin form post as Origin: null. Page script
			// cannot set Sec-Fetch-Site, and an opaque or other-site initiator gets cross-site.
			if origin == "null" && r.Header.Get("Sec-Fetch-Site") == "same-origin" {
				next.ServeHTTP(w, r)
				return
			}
			if origin == "" || originAllowed(origin, r, allowed) {
				next.ServeHTTP(w, r)
				return
			}
			WriteError(w, http.StatusForbidden, "cross-origin request rejected")
		})
	}
}

func originAllowed(origin string, r *http.Request, allowed []string) bool {
	for _, a := range allowed {
		if a != "" && strings.EqualFold(origin, a) {
			return true
		}
	}
	// Fall back to the Host the request arrived on, so a deployment that never
	// configures an origin still accepts its own console.
	if host := r.Host; host != "" {
		if strings.EqualFold(origin, "https://"+host) ||
			strings.EqualFold(origin, "http://"+host) {
			return true
		}
	}
	return false
}

// limiter is a fixed-window rate limiter keyed by an arbitrary string.
//
// Sign-in is the endpoint that most needs this: it is public by necessity, and
// every attempt costs a bcrypt comparison. Unthrottled that is simultaneously a
// credential-stuffing endpoint and a CPU-exhaustion amplifier — the server does
// ~80ms of work for an attacker's near-zero cost.
//
// A fixed window is coarser than a token bucket and can allow a burst across a
// boundary. That is an acceptable trade for something with no dependencies and
// no background goroutine, because the goal is to make automated guessing
// impractical, not to enforce an exact rate.
type limiter struct {
	mu     sync.Mutex
	hits   map[string]*window
	limit  int
	window time.Duration
}

type window struct {
	count int
	reset time.Time
}

func newLimiter(limit int, per time.Duration) *limiter {
	return &limiter{hits: map[string]*window{}, limit: limit, window: per}
}

// allow records an attempt and reports whether it may proceed.
func (l *limiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// Evict expired entries opportunistically. Without this the map grows with
	// every distinct source address, which is itself a memory-exhaustion vector
	// on a public port.
	if len(l.hits) > 8192 {
		for k, w := range l.hits {
			if now.After(w.reset) {
				delete(l.hits, k)
			}
		}
	}

	w, ok := l.hits[key]
	if !ok || now.After(w.reset) {
		l.hits[key] = &window{count: 1, reset: now.Add(l.window)}
		return true
	}
	w.count++
	return w.count <= l.limit
}

// retryAfter reports how long the caller should wait, for the Retry-After header.
func (l *limiter) retryAfter(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w, ok := l.hits[key]; ok {
		if d := time.Until(w.reset); d > 0 {
			return d
		}
	}
	return l.window
}

// clientIP extracts the address to rate-limit on.
//
// X-Forwarded-For is only consulted when the operator has said a trusted proxy
// sits in front, because otherwise it is attacker-controlled: a client that can
// set its own key can rotate it and defeat the limiter entirely. When trusted,
// the RIGHTMOST entry is used — the leftmost entries are whatever the client
// sent, and only the last hop was appended by our own proxy.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
		if rip := r.Header.Get("X-Real-IP"); rip != "" {
			return strings.TrimSpace(rip)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimit throttles a handler by client address.
func rateLimit(l *limiter, trustProxy bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := clientIP(r, trustProxy)
		if !l.allow(key) {
			w.Header().Set("Retry-After",
				strconv.Itoa(int(l.retryAfter(key).Seconds())+1))
			WriteError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// maxBody caps a request body before it is decoded.
//
// Every JSON handler decodes straight from r.Body, so without a cap an
// unauthenticated caller can stream gigabytes into a decode buffer. The limit is
// generous for a prompt and small enough that it cannot be used as a memory
// exhaustion primitive.
const maxJSONBody = 1 << 20 // 1 MiB

func capBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
}

// canonicalHost redirects any request that names a host other than the
// configured one. It runs outermost, before origin checks and cookies, so a
// visitor who still types the old name lands on the new one without ever
// having a cookie set for the old host. Only GET and HEAD are redirected: a
// browser would replay a POST against the new host as a GET, silently
// dropping its body, and a script sending state to the wrong name should
// learn that from a refusal rather than a quiet detour.
func canonicalHost(host string, next http.Handler) http.Handler {
	if host == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Host, host) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "this deployment is served at https://"+host, http.StatusMisdirectedRequest)
			return
		}
		u := *r.URL
		u.Scheme = "https"
		u.Host = host
		http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
	})
}
