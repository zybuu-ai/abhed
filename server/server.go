// Package server exposes Abhed as a multi-user service.
//
// The CLI and the web console consume the SAME event stream (docs §10): there
// is one agent loop implementation, and the server is a transport over it, not
// a second product. That is what keeps the two surfaces from drifting.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/hawkeye"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/docsite"
	"github.com/zybuu-ai/abhed/internal/index"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/skills"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// EventStore is what the server needs from a store: durable append plus live
// subscription. Both the in-memory and Postgres stores satisfy it, so server
// code never branches on the backend.
type EventStore interface {
	agent.Store
	Subscribe(sessionID string) <-chan agent.Event
	Unsubscribe(sessionID string, ch <-chan agent.Event)
}

// SessionRecorder is implemented by durable stores that track session rows.
// Optional: the memory store does not, and the server degrades gracefully.
type SessionRecorder interface {
	CreateSession(ctx context.Context, s store.SessionRecord) error
	ListSessions(ctx context.Context, limit int) ([]store.SessionRecord, error)
}

// SessionResumer is implemented by stores that can hand a finished session
// to exactly one process for continuation. Without it, a session that ended
// with a server process stays ended; with it, any node can pick any session
// up from the record.
type SessionResumer interface {
	ClaimResume(ctx context.Context, sessionID string) (bool, error)
}

// SessionRouter is implemented by stores that can record which node holds a
// session's turn in flight.
//
// A turn lives in one process's memory — the loop, its cancel function and
// the channel a pending approval waits on — so a request about a running
// session has to reach that process. Behind one server this is free; behind
// several it is the difference between an approval arriving and vanishing.
//
// Optional: without it the server behaves exactly as before, which is correct
// for a single node.
type SessionRouter interface {
	ClaimNode(ctx context.Context, sessionID, nodeID string) error
	ReleaseNode(ctx context.Context, sessionID, nodeID string) error
	NodeFor(ctx context.Context, sessionID string, stale time.Duration) (string, error)
}

// ApprovalStore is implemented by stores that can hold a pending approval
// durably, so a reviewer's answer reaches the waiting turn from any node.
//
// Without it the exchange stays in memory, which is correct for one server
// and loses the answer behind a load balancer.
type ApprovalStore interface {
	AskApproval(ctx context.Context, a store.Approval) (string, error)
	AnswerApproval(ctx context.Context, id string, approved bool, by string) (bool, error)
	ApprovalResult(ctx context.Context, id string) (approved, answered bool, err error)
	PendingApproval(ctx context.Context, sessionID string) (store.Approval, bool, error)
}

// approvalPoll is how often a waiting turn checks the database for an answer.
// Short enough that a reviewer does not notice the delay, long enough that a
// thirty-minute wait is not thousands of queries.
const approvalPoll = 2 * time.Second

// nodeStale is how long a claim survives without being refreshed. Longer than
// any turn boundary, short enough that a node which died does not strand its
// sessions for long.
const nodeStale = 2 * time.Minute

// nodeHeartbeat refreshes the claim well inside nodeStale, so a slow write or
// a missed tick does not make a healthy node look dead.
const nodeHeartbeat = 30 * time.Second

// Mount registers routes on the server's mux. It runs after the built-in
// routes and before the middleware wraps the mux, so the handlers it
// registers are authenticated and logged like any other; a route that must
// be reachable before sign-in is declared public by the provider that owns
// it, not here.
type Mount func(s *Server, mux *http.ServeMux)

// InviteRedeemer admits a registration by code when open signup is off.
//
// The server owns the signup handler because it owns the accounts; who gets
// a code, how long it lasts and what record sits behind it are an edition's
// business. Redeem is the gate: it consumes the code or says why it cannot.
// Redeemed is told afterwards, once the account exists, so whatever issued
// the code can name the account it became — after, not before, because a
// failed CreateUser must not leave a record claiming an account that does
// not exist.
type InviteRedeemer interface {
	Redeem(ctx context.Context, code, username string) error
	Redeemed(ctx context.Context, code, username string) error
}

// TenantResolver decides which tenant a request acts in, given the identity
// the auth layer established (nil when there is none). The server is
// single-store: it never switches tenant mid-request, and the resolver's
// answer is what the store's row-level security is checked against. A
// multi-tenant arrangement runs one server per tenant behind a router that
// authenticates once; the resolver is how that router's answer reaches here.
type TenantResolver interface {
	Tenant(ctx context.Context, id *auth.Identity) string
}

type Options struct {
	Addr      string
	Workspace string
	// NodeID identifies this process among several behind a load balancer.
	// Empty means a single-node deployment: nothing is claimed and routing
	// stays off, which is the right default.
	NodeID string
	// HomeURL, when set, is linked from the console and the sign-in page as
	// the way back to whoever operates this deployment.
	//
	// Empty by default, and that default is the important part: an air-gapped
	// install has no route to the internet, so a hardcoded link there is a
	// dead end rather than a courtesy. A public deployment sets it; an
	// enclave leaves it unset and the link does not render at all.
	HomeURL  string
	Config   config.Config
	Adapter  model.Adapter
	Registry *tools.Registry
	// Redact rewrites every event payload before it is written; the app sets
	// it from the secrets store. Nil records payloads as they are.
	Redact agent.Redactor
	// SkillListing is the rendered skill index for the system prompt. The
	// server takes the rendered string rather than the registry, because the
	// registry's only other use is the tool, which is already in Registry.
	SkillListing string
	// SkillDirs are the loaded skills' directories, granted to every session
	// so a skill can reference the scripts and assets shipped beside it.
	SkillDirs []string
	Logger    *slog.Logger
	// Store defaults to an in-memory store when nil.
	Store EventStore
	// EventTap sees every event as it is appended, on the appending
	// goroutine. It must return immediately; the telemetry exporter honours
	// that by queueing and dropping rather than waiting. Nil means no tap.
	EventTap func(agent.Event)
	// Auth verifies callers. Nil means the mode from Config is used.
	Auth *auth.Middleware
	// Mounts add routes to the server's mux before it is wrapped, so a
	// mounted route gets identity, logging, panic recovery, the origin check
	// and the body cap on the same terms as a built-in one. This is how an
	// edition adds its own surface — an access dashboard, a scheduler's admin
	// routes — without the server knowing what it is.
	Mounts []Mount
	// AdminURL is where the console's Admin link points. Empty hides the
	// link: the settings and user routes stay, but the page that fronts them
	// is a mount, and a link to a page nobody registered would answer 404.
	AdminURL string
	// Invites admits registrations by code when allow_signup is off. Nil
	// means registration is open only when allow_signup says so; the
	// landing page offers a code field only when this is set.
	Invites InviteRedeemer
	// Tenant decides which tenant a request acts in. Nil means the default
	// rule: the configured storage tenant when authentication is off, the
	// identity's tenant otherwise.
	Tenant TenantResolver
	// SkillRoots are the directories skills are looked FOR in — distinct from
	// SkillDirs, which holds each loaded skill's own directory so its assets
	// can be read. A reload has to scan the roots.
	SkillRoots []string
	// SkillRegistry is the loaded skill set. Held alongside SkillListing so a
	// settings change can re-render the listing rather than being stuck with
	// the string computed at startup.
	SkillRegistry *skills.Registry
	// Gateway holds the MCP connections, so a server can be added at runtime.
	Gateway *mcp.Gateway
	// Index backs the retrieval tool, for a reindex triggered from settings.
	Index        *index.Index
	IndexOptions index.BuildOptions
	// DrainTimeout is how long a shutdown waits for running turns to finish
	// before cancelling them. Zero keeps the old behaviour of ending them at
	// once, which is what a single-node deployment with no balancer wants.
	DrainTimeout time.Duration
}

// Server holds live sessions and serves the API.
type Server struct {
	opts     Options
	store    EventStore
	sessions SessionRecorder // nil when the store is not durable
	log      *slog.Logger
	mu       sync.RWMutex
	running  map[string]*liveSession
	// draining is set once shutdown starts: running turns finish, new ones
	// are refused so a balancer sends them to a node that can take them.
	draining atomic.Bool

	// Throttles for the endpoints reachable before authentication succeeds.
	signinLimiter  *limiter
	sessionLimiter *limiter

	// mounts added after construction, ahead of the ones in Options.
	mounts []Mount

	// adminMu serialises admin-rights changes, so two demotions at once
	// cannot leave nobody an administrator.
	adminMu sync.Mutex

	// state holds what a settings change may replace, behind its own lock.
	// Separate from opts, which stays immutable — mixing "set once" and
	// "changes at runtime" in one struct is how a field ends up read without
	// the lock.
	state *mutable
}

type liveSession struct {
	ID      string
	User    string
	Tenant  string
	Loop    *agent.Loop
	Cancel  context.CancelFunc
	Created time.Time
	Prompt  string
	State   string // running | waiting_approval | done
	Turns   int    // exchanges in this conversation
	cancel  context.CancelFunc
	// ran is closed when the current run's goroutine ends, so a caller that
	// interrupted it can wait for the loop to be free.
	ran chan struct{}
	// cancelCause ends the run with a stated reason, so shutdown is not
	// recorded as a user interrupt.
	cancelCause context.CancelCauseFunc
	approvals   chan approvalReply
	pending     *pendingApproval
	// durable, when set, records the approval so an answer arriving at
	// another node still reaches this turn. Nil keeps the in-memory
	// behaviour, which is right for a single server.
	durable ApprovalStore
	// allowed holds scopes the reviewer chose to "always allow" for this
	// session, so default mode stops re-prompting for the same kind of call.
	// It mirrors the CLI's session AllowList; without it the console asked
	// again on every mutating tool with no way to say "don't ask again".
	allowed map[string]bool
	// undo holds each file's content from before the agent first changed it,
	// which is what the console's changes view diffs against.
	undo *agent.UndoLog
	// manual is the tool session of the person at the workbench, and manualMu
	// runs their calls one at a time so two commands never share a cd.
	manual   *tools.Session
	manualMu sync.Mutex
	// ptys are the person's commands running on a terminal.
	ptys map[string]*ptyRun
	mu   sync.Mutex
}

type pendingApproval struct {
	EventID string          `json:"event_id"`
	Tool    string          `json:"tool"`
	Args    json.RawMessage `json:"args"`
	Reason  string          `json:"reason"`
	Scope   string          `json:"scope"`
}

type approvalReply struct {
	Approved bool
	Scope    string
}

func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	st := opts.Store
	// The tap wraps only what the loop writes through. Optional interfaces
	// (session recording, deletion, access records) are asserted on the
	// unwrapped store below, so tapping cannot silently switch them off.
	tapped := st
	if st == nil {
		st = agent.NewMemStore()
		tapped = st
	}
	if opts.EventTap != nil {
		tapped = tapStore{EventStore: st, tap: opts.EventTap}
	}
	s := &Server{
		opts:    opts,
		store:   tapped,
		log:     opts.Logger,
		running: make(map[string]*liveSession),
		// Ten sign-in attempts a minute is far beyond what a person typing a
		// password needs, and far below what makes guessing viable.
		signinLimiter:  newLimiter(10, time.Minute),
		sessionLimiter: newLimiter(60, time.Minute),
		state: &mutable{
			registry: opts.Registry,
			skills:   opts.SkillRegistry,
			gateway:  opts.Gateway,
			cfg:      opts.Config,
		},
	}
	if rec, ok := st.(SessionRecorder); ok {
		s.sessions = rec
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("GET /v1/sessions/{id}/events", s.streamEvents)
	mux.HandleFunc("GET /v1/sessions/{id}/replay", s.replaySession)
	mux.HandleFunc("GET /v1/sessions/{id}/hawkeye", s.hawkeyeSession)
	mux.HandleFunc("GET /v1/sessions/{id}/tree", s.treeSession)
	mux.HandleFunc("GET /v1/sessions/{id}/file", s.fileSession)
	mux.HandleFunc("GET /v1/sessions/{id}/changes", s.changesSession)
	mux.HandleFunc("PUT /v1/sessions/{id}/file", s.saveFile)
	mux.HandleFunc("POST /v1/sessions/{id}/exec", s.execCommand)
	mux.HandleFunc("GET /v1/sessions/{id}/original", s.originalFile)
	mux.HandleFunc("POST /v1/sessions/{id}/accept", s.acceptChange)
	mux.HandleFunc("POST /v1/sessions/{id}/pty", s.startPTY)
	mux.HandleFunc("GET /v1/sessions/{id}/pty/{pty}", s.streamPTY)
	mux.HandleFunc("POST /v1/sessions/{id}/pty/{pty}/input", s.writePTY)
	mux.HandleFunc("POST /v1/sessions/{id}/pty/{pty}/resize", s.resizePTY)
	mux.HandleFunc("DELETE /v1/sessions/{id}/pty/{pty}", s.killPTY)
	mux.HandleFunc("POST /v1/sessions/{id}/messages", s.postMessage)
	mux.HandleFunc("GET /v1/sessions/{id}/queue", s.listQueue)
	mux.HandleFunc("DELETE /v1/sessions/{id}/queue/{qid}", s.cancelQueued)
	mux.HandleFunc("POST /v1/sessions/{id}/upload", s.uploadFile)
	// Uploading before a session exists: see uploadFile for why a placeholder
	// session was the wrong answer.
	mux.HandleFunc("POST /v1/uploads", s.uploadFile)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.deleteSession)

	// Administrative routes, gated per-route on group membership rather than
	// by wrapping the whole mux — see rbac.go for why that distinction
	// matters. mux.Handle rather than HandleFunc because each is wrapped.
	mux.Handle("GET /v1/admin/users", s.Admin(s.listUsers))
	mux.Handle("POST /v1/admin/users/admin", s.Admin(s.setUserAdmin))
	mux.Handle("GET /v1/admin/settings", s.Admin(s.getSettings))
	mux.Handle("POST /v1/admin/skills/reload", s.Admin(s.reloadSkills))
	mux.Handle("POST /v1/admin/mcp", s.Admin(s.addMCP))
	mux.Handle("POST /v1/admin/reindex", s.Admin(s.reindex))
	mux.HandleFunc("GET /v1/sessions/{id}/files", s.listDownloads)
	mux.HandleFunc("GET /v1/sessions/{id}/download", s.serveDownload)
	mux.HandleFunc("GET /v1/providers", s.listProviders)
	mux.HandleFunc("POST /v1/sessions/{id}/model", s.setSessionModel)
	mux.HandleFunc("POST /v1/sessions/{id}/interrupt", s.interruptSession)
	mux.HandleFunc("POST /v1/sessions/{id}/approve", s.approveAction)
	mux.HandleFunc("GET /v1/health", s.health)

	// Each provider registers the routes it owns: local accounts and an
	// identity provider are not mutually exclusive, and the server does not
	// know which are present. What the server registers itself is the part
	// that is the same whoever holds the session — sign-out has to clear
	// whichever session the browser actually holds, and whoami has to
	// answer for it.
	providers := s.signIns()
	for _, p := range providers {
		p.Routes(mux)
	}
	switch {
	case len(providers) > 0:
		mux.HandleFunc("GET /logout", s.signOut)
		mux.HandleFunc("GET /v1/whoami", s.whoami)
		if s.LocalAuth() != nil {
			// Registered whenever local accounts exist. The handler decides
			// admission: open, invite-only, or closed. Registering it
			// conditionally would make "invite-only" impossible without a
			// restart, which is the case that matters most.
			mux.HandleFunc("POST /v1/signup", s.signup)
		}
		if !hasEntryPoint(providers) {
			// No provider has a page of its own, so the form is on "/". The
			// path still answers, for the bookmark and the redirect that
			// expect it.
			mux.HandleFunc("GET /login", s.redirectHome)
		}
	default:
		// Sign-in is not configured. These routes still answer, because a 404
		// leaves the console unable to tell "no auth here" from "the server is
		// broken" — and a user who clicks Sign out deserves an explanation
		// rather than a Go 404 page.
		mux.HandleFunc("GET /v1/whoami", s.whoamiDisabled)
		mux.HandleFunc("GET /login", s.authDisabledPage)
		mux.HandleFunc("GET /logout", s.authDisabledPage)
	}
	mux.HandleFunc("GET /", s.serveLanding)
	mux.HandleFunc("GET /console", s.serveConsole)
	mux.HandleFunc("GET /ide", s.serveIDE)
	mux.HandleFunc("GET /account", s.serveAccount)
	mux.HandleFunc("GET /favicon.ico", serveFavicon)
	mux.HandleFunc("GET /favicon.svg", serveFavicon)
	mux.HandleFunc("GET /ide/vendor/{file}", s.serveIDEVendor)
	mux.HandleFunc("GET /v1/capabilities", s.getCapabilities)

	// Documentation, when it was embedded at build time. An air-gapped
	// install has no route to the public copy, so the binary carries its own;
	// a build that skipped generation simply has no /docs rather than a route
	// that 404s every page. Registered before the auth wrapper below because
	// documentation is not a secret and an operator who cannot sign in is
	// exactly who needs to read it.
	if docsite.Available() {
		mux.Handle("GET /docs/", docsite.Handler("/docs"))
		mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
		})
	}
	mux.HandleFunc("GET /v1/overview", s.overview)

	// Mounted last, so a mount sees the finished mux and its routes are
	// wrapped below with everyone else's.
	for _, m := range s.mounts {
		m(s, mux)
	}
	for _, m := range s.opts.Mounts {
		m(s, mux)
	}

	// Order matters and is easy to get backwards: authentication must run
	// BEFORE the layer that reads the identity, so it wraps closest to the
	// outside. An inverted order silently yields anonymous identities.
	var handler http.Handler = mux
	if s.opts.Config.Auth.RequireGroup != "" {
		handler = auth.RequireGroup(s.opts.Config.Auth.RequireGroup, handler)
	}
	handler = s.withMiddleware(handler) // reads identity, logs

	// Sign-in is throttled OUTSIDE authentication, because an unauthenticated
	// attacker is precisely who this limits: by the time the auth layer has
	// rejected a password, the bcrypt comparison has already been paid for.
	authed := s.authMiddleware().Wrap(handler) // establishes identity
	limited := s.throttle(authed)

	// Origin is checked before anything reads a cookie, and headers are set
	// outermost so they are present on rejections too — an error response is
	// still a response a browser will act on.
	guarded := sameOrigin(s.opts.Config.Server.AllowedOrigins)(limited)
	headed := securityHeaders(s.bodyLimit(guarded), s.opts.Config.Server.HSTS)
	return canonicalHost(s.opts.Config.Server.CanonicalHost, headed)
}

// PublicPaths answer before anyone signs in, whichever providers are
// configured; each provider adds the paths it owns. Kept short on purpose.
func PublicPaths() []string {
	return []string{"/", "/v1/health", "/v1/overview", "/login", "/logout", "/v1/whoami", "/favicon.ico", "/favicon.svg"}
}

// Mount adds routes for the next Handler call, for a caller that has the
// constructed server in hand rather than its Options — a hook that binds a
// scheduler to this server and then wants to expose its status, say. It must
// run before the handler is built; a mount added afterwards is not served.
func (s *Server) Mount(m Mount) { s.mounts = append(s.mounts, m) }

// signIns are the browser sign-in providers the auth layer was given.
func (s *Server) signIns() []auth.Provider {
	if s.opts.Auth == nil {
		return nil
	}
	return s.opts.Auth.Providers
}

// hasEntryPoint reports whether any provider owns a sign-in page.
func hasEntryPoint(ps []auth.Provider) bool {
	for _, p := range ps {
		if u, _ := p.SignIn(); u != "" {
			return true
		}
	}
	return false
}

// bodyLimit caps every request body before any handler decodes it.
//
// Applied centrally rather than per-handler: there are six JSON decode sites
// today, and the one that gets added next month is the one that would have been
// forgotten. Uploads, and a file saved or accepted from the workbench, set
// their own larger limit inside their handlers, so they are exempted here
// rather than being clamped to the JSON size.
func (s *Server) bodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		own := strings.HasSuffix(r.URL.Path, "/upload") || r.URL.Path == "/v1/uploads" ||
			(r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/file")) ||
			strings.HasSuffix(r.URL.Path, "/accept")
		if r.Body != nil && !own {
			capBody(w, r)
		}
		next.ServeHTTP(w, r)
	})
}

// throttle rate-limits the endpoints an anonymous caller can reach.
//
// Only the credential endpoints are limited, not the whole API: a signed-in
// user driving an agent legitimately makes many requests, and throttling those
// would degrade normal use to defend against an attacker who is already past
// the door.
func (s *Server) throttle(next http.Handler) http.Handler {
	trustProxy := s.opts.Config.Auth.Mode == "proxy" || s.opts.Config.Server.TrustProxy
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/signin", "/v1/signup", "/v1/password":
			rateLimit(s.signinLimiter, trustProxy, next).ServeHTTP(w, r)
		case "/v1/sessions":
			// Creating a session starts an agent loop, which is the most
			// expensive thing this server does. It is authenticated, so the
			// limit is generous — it exists to bound a runaway client, not to
			// police normal use.
			if r.Method == http.MethodPost {
				rateLimit(s.sessionLimiter, trustProxy, next).ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// authMiddleware builds the identity layer from config unless one was injected.
func (s *Server) authMiddleware() auth.Middleware {
	if s.opts.Auth != nil {
		return *s.opts.Auth
	}
	// Sign-in itself must be reachable without being signed in, or the only
	// way in is barred by the thing it unlocks.
	mw := auth.Middleware{PublicPaths: append(PublicPaths(), "/auth/callback", "/v1/signin", "/v1/signup")}
	if s.opts.Config.Auth.Mode == "proxy" {
		mw.TrustHeaders = true
	}
	return mw
}

// withMiddleware applies identity and logging. Authentication is delegated to
// the enterprise IdP in production (docs/ops/air-gap.md §4); this reads the
// identity headers a reverse proxy sets after authenticating.
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Identity comes from the auth layer, which has already verified it.
		user := "anonymous"
		id, _ := auth.FromContext(r.Context())
		if id != nil {
			user = id.Subject
			if id.Email != "" {
				user = id.Email
			}
		}
		tenant := s.tenantFor(r.Context(), id)
		ctx := context.WithValue(r.Context(), ctxUser, user)
		ctx = context.WithValue(ctx, ctxTenant, tenant)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// A panicking handler would otherwise drop the connection with no
		// status and no audit line — the request simply vanishes from the log,
		// which is the worst possible outcome for something internet-facing.
		// The client is told nothing beyond "internal error": a Go stack trace
		// names packages, paths, and versions.
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic serving request",
					"method", r.Method, "path", r.URL.Path,
					"user", user, "panic", v)
				if rec.status == http.StatusOK && !rec.wrote {
					WriteError(rec, http.StatusInternalServerError, "internal error")
				}
			}
		}()

		next.ServeHTTP(rec, r.WithContext(ctx))

		s.log.Info("request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"user", user, "tenant", tenant, "duration", time.Since(start))
	})
}

type ctxKey string

const (
	ctxUser   ctxKey = "user"
	ctxTenant ctxKey = "tenant"
)

// UserOf is the caller the middleware resolved, for a handler that records
// who did something. Anonymous when authentication is off.
func UserOf(ctx context.Context) string {
	if v, ok := ctx.Value(ctxUser).(string); ok {
		return v
	}
	return "anonymous"
}

// TenantOf is the tenant the middleware resolved for this request.
func TenantOf(ctx context.Context) string {
	if v, ok := ctx.Value(ctxTenant).(string); ok {
		return v
	}
	return "default"
}

// tenantFor applies the configured resolver, or the default rule.
func (s *Server) tenantFor(ctx context.Context, id *auth.Identity) string {
	if s.opts.Tenant != nil {
		return s.opts.Tenant.Tenant(ctx, id)
	}
	tenant := "default"
	if id != nil && id.Tenant != "" {
		tenant = id.Tenant
	}
	return storeTenant(s.opts.Config, tenant)
}

// HomeURL is the operator's site link, for a mounted page that renders the
// same header as the built-in ones.
func (s *Server) HomeURL() string { return s.opts.HomeURL }

// Logger is the server's logger, for a mounted handler that should log the
// way built-in ones do.
func (s *Server) Logger() *slog.Logger { return s.log }

// Config is the configuration the server was built with.
func (s *Server) Config() config.Config { return s.opts.Config }

type statusRecorder struct {
	http.ResponseWriter
	status int
	// wrote tracks whether anything reached the client, so panic recovery can
	// tell "nothing was sent, send a 500" from "a response was already
	// streaming", where a second WriteHeader would only log a superfluous-call
	// warning and corrupt the body.
	wrote bool
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

// Flush lets SSE writes reach the client through the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type createRequest struct {
	Prompt string `json:"prompt"`
	Mode   string `json:"mode,omitempty"`
	// Provider names one of the CONFIGURED providers. Never a URL or a key —
	// see provider.go for why that distinction is load-bearing.
	Provider string `json:"provider,omitempty"`
	// Interrupt, on a follow-up to a busy session, stops the running turn and
	// sends this message as a fresh one instead of queueing it.
	Interrupt bool `json:"interrupt,omitempty"`
	// ClientID is the sender's own id for the message, echoed on the
	// user.message that records it.
	ClientID string `json:"client_id,omitempty"`
}

// validClientID keeps a client's id short and plain, since it is recorded.
func validClientID(id string) bool {
	if len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

type createResponse struct {
	SessionID string `json:"session_id"`
}

// StartSpec is everything session creation needs, independent of where the
// request came from. HTTP fills it from the request and its identity; a
// scheduler fills it from a config entry and the clock. Both paths run the
// same code, so a scheduled run is an ordinary session in every way except
// who started it.
type StartSpec struct {
	Prompt   string
	Mode     string
	Provider string
	// ClientID is echoed on the first user.message; see createRequest.
	ClientID string
	User     string
	Tenant   string
	// Unattended means nobody can answer an approval. An "ask" under the
	// configured mode becomes a denial, recorded like any other; a caller
	// that wants an unattended run to edit files gives it a mode or allow
	// rules that need no person — a choice made in config, on the record,
	// not a default made here. Attended runs park on the live session until
	// a person answers in the console.
	Unattended bool
	// OnEnd is called when the run finishes, however it finishes, with the
	// terminal reason as the event stream records it.
	OnEnd func(reason string, err error)
}

// errBadMode is returned by StartSession when the requested mode would widen
// permissions; the HTTP handler turns it into a 403.
var errBadMode = errors.New("mode may only narrow permissions; a client may request \"plan\" and nothing else")

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		WriteError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if !validClientID(req.ClientID) {
		WriteError(w, http.StatusBadRequest, "client_id is up to 64 letters, digits, - and _")
		return
	}
	sessionID, err := s.StartSession(r.Context(), StartSpec{
		Prompt: req.Prompt, Mode: req.Mode, Provider: req.Provider, ClientID: req.ClientID,
		User: UserOf(r.Context()), Tenant: TenantOf(r.Context()),
	})
	if err != nil {
		switch {
		case errors.Is(err, errBadMode):
			WriteError(w, http.StatusForbidden, err.Error())
		case strings.HasPrefix(err.Error(), "provider:"):
			WriteError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "provider: "))
		default:
			WriteError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	WriteJSON(w, http.StatusAccepted, createResponse{SessionID: sessionID})
}

// StartSession persists, builds and starts a session, returning once the
// agent is running. The context passed in governs creation only; the run
// itself gets its own, cancelled through the live session.
func (s *Server) StartSession(ctx context.Context, spec StartSpec) (string, error) {
	// The mode is resolved before anything is persisted or started, because a
	// rejected mode must not leave a half-created session behind.
	mode, ok := requestMode(s.opts.Config.Permissions.Mode, spec.Mode)
	if !ok {
		return "", errBadMode
	}

	// One snapshot for the whole of session creation. Taking it once means a
	// settings change landing mid-request cannot give this session a tool
	// registry from before the change and a skill listing from after it.
	registry, skillReg, _ := s.state.snapshot()

	// Resolved before anything is persisted or started: a session half-created
	// against a provider that does not exist is worse than a clean refusal.
	adapter := s.opts.Adapter
	if spec.Provider != "" {
		a, err := s.resolveProvider(spec.Provider)
		if err != nil {
			return "", fmt.Errorf("provider: %w", err)
		}
		adapter = a
	}

	sessionID := newSessionID()

	// Events reference sessions, so the session row must exist first.
	if s.sessions != nil {
		if err := s.sessions.CreateSession(ctx, store.SessionRecord{
			ID: sessionID,
			// Leave Tenant empty so the store applies its own configured
			// tenant. With auth.mode=none every request is "default", which
			// would otherwise collide with a store scoped to a real tenant and
			// fail row-level security on the very first session.
			Tenant:    storeTenant(s.opts.Config, spec.Tenant),
			User:      spec.User,
			Workspace: s.opts.Workspace,
			Model:     adapter.Profile().Name,
			Mode:      mode,
			Prompt:    spec.Prompt,
			StartedAt: time.Now().UTC(),
		}); err != nil {
			return "", fmt.Errorf("persist session: %w", err)
		}
	}

	rec := agent.NewRecorder(s.store, sessionID, "")
	rec.Redact = s.opts.Redact
	live, loop, err := s.buildLive(sessionID, spec, mode, adapter, registry, skillReg, rec)
	if err != nil {
		return "", err
	}

	runCtx, cancelCause := context.WithCancelCause(context.Background())
	cancel := func() { cancelCause(nil) }
	live.Cancel = cancel
	live.cancel = cancel
	live.cancelCause = cancelCause
	live.Turns = 1
	live.ran = make(chan struct{})

	if s.draining.Load() {
		return "", errDraining
	}
	s.mu.Lock()
	s.running[sessionID] = live
	s.mu.Unlock()
	s.claimNode(ctx, sessionID)
	stopBeat := s.heartbeatNode(runCtx, sessionID)

	go func() {
		defer cancel()
		live.undo.BeginTurn()
		reason, err := loop.RunMessage(runCtx, agent.Message{Text: spec.Prompt, ClientID: spec.ClientID})
		for live.settle(runCtx, reason, err) {
			reason, err = loop.RunQueued(runCtx)
		}
		stopBeat()
		s.releaseNode(sessionID)
		if spec.OnEnd != nil {
			spec.OnEnd(string(reason), err)
		}
		if err != nil {
			s.log.Error("session failed", "session", sessionID, "error", err)
			return
		}
		s.log.Info("session ended", "session", sessionID, "reason", reason)
	}()

	return sessionID, nil
}

// newPolicy builds the engine from the operator's rules. One constructor, so
// a rule cannot bind the agent and not the endpoints that serve files.
func (s *Server) newPolicy(mode policy.Mode) *policy.Engine {
	pol := policy.New(mode)
	pol.Managed = s.opts.Config.Managed
	_ = pol.AddDeny(s.opts.Config.Permissions.Deny...)
	_ = pol.AddAsk(s.opts.Config.Permissions.Ask...)
	_ = pol.AddAllow(s.opts.Config.Permissions.Allow...)
	return pol
}

// buildLive constructs the in-process session and its loop: the scoped
// workspace, the policy from config, the system prompt, the approver. One
// function for both a new session and a continued one, so the two cannot
// drift in what they permit.
func (s *Server) buildLive(sessionID string, spec StartSpec, mode string, adapter model.Adapter,
	registry *tools.Registry, skillReg *skills.Registry, rec *agent.Recorder) (*liveSession, *agent.Loop, error) {

	sess, err := tools.NewSession(s.opts.Workspace)
	if err != nil {
		return nil, nil, err
	}
	if sess.Syntax, err = tools.ParseSyntaxMode(s.opts.Config.Tools.SyntaxCheck); err != nil {
		return nil, nil, err
	}
	// Extra roots come from the operator's config, applied to every session.
	// A refusal here is a misconfiguration, not a per-request problem: fail
	// the session rather than silently running with a narrower scope than the
	// operator asked for.
	dirs := append([]string{}, s.opts.Config.AdditionalDirs...)
	// A skill's own directory is reachable: its instructions reference files
	// beside them, and denying that read is a dead end for the agent.
	dirs = append(dirs, s.opts.SkillDirs...)
	for _, dir := range dirs {
		if err := sess.AddRoot(dir); err != nil {
			return nil, nil, fmt.Errorf("additional_dirs: %w", err)
		}
	}

	pol := s.newPolicy(policy.Mode(mode))
	undo := agent.NewUndoLog()
	sess.Checkpoint = undo.Record

	live := &liveSession{
		ID: sessionID, User: spec.User, Tenant: spec.Tenant,
		Created: time.Now(), Prompt: spec.Prompt, State: "running",
		approvals: make(chan approvalReply, 1),
		allowed:   map[string]bool{},
		durable:   s.approvalStore(),
		undo:      undo,
	}

	cfg := agent.DefaultConfig()
	cfg.SystemPrompt = agent.BuildSystemPrompt(agent.BuildOptions{
		Profile:       "main",
		Workspace:     s.opts.Workspace,
		Model:         adapter.Profile().Name,
		ContextWindow: adapter.Profile().ContextWindow,
		MemoryFiles:   agent.DiscoverMemoryFiles(s.opts.Workspace),
		Skills:        s.skillListing(skillReg),
	})
	cfg.MaxTurns = s.opts.Config.Limits.MaxTurns
	// The server built its loops on the defaults and ignored the operator's
	// context settings; the CLI has always honoured them.
	if at := s.opts.Config.Context.CompactAt; at > 0 {
		cfg.CompactAt = at
	}
	cfg.OffloadAt = s.opts.Config.Context.OffloadFraction()

	var approver agent.Approver = live
	if spec.Unattended {
		approver = agent.AutoApprove{Yes: false}
	}
	loop := agent.NewLoop(adapter, registry, pol, approver, sess, rec, cfg)
	loop.Compactor = agent.NewCompactor(adapter, cfg.CompactAt)
	loop.Budget = agent.NewBudget(
		int64(s.opts.Config.Limits.MaxBudgetTokens),
		s.opts.Config.Limits.MaxSubagents,
		s.opts.Config.Limits.NestedSubagents,
	)
	live.Loop = loop
	return live, loop, nil
}

// resumeSession continues a finished session from its record, on this node.
//
// The conversation is rebuilt from the event log the same way a fork is, the
// recorder is advanced past the events already stored, and the store is asked
// to claim the session so that only one process continues it. The prompt is
// then applied as a continuation: the turn budget carries over, the policy is
// the one this deployment runs now, and every new event lands in the same
// sequence as the old ones. This is what lets a session outlive the process
// that started it — and, behind a load balancer, the node.
func (s *Server) resumeSession(ctx context.Context, id string, prompt, user, tenant string) (*liveSession, error) {
	if s.draining.Load() {
		return nil, errDraining
	}
	events, err := s.store.Events(id)
	if err != nil {
		return nil, fmt.Errorf("read record: %w", err)
	}
	if len(events) == 0 {
		return nil, errNoSession
	}
	// Ownership and mode come from the stored row when there is one.
	var rec store.SessionRecord
	if s.sessions != nil {
		recs, err := s.sessions.ListSessions(ctx, 500)
		if err != nil {
			return nil, fmt.Errorf("read sessions: %w", err)
		}
		found := false
		for _, r := range recs {
			if r.ID == id {
				rec, found = r, true
			}
		}
		if !found {
			return nil, errNoSession
		}
	}
	if s.sessions != nil && !ownsSession(rec.Tenant, rec.User, tenant, user) {
		return nil, errNoSession
	}

	// Exactly one continuer. A durable store arbitrates; without one there
	// is only this process, and the live map is the arbiter.
	if claimer, ok := s.sessions.(SessionResumer); ok {
		claimed, err := claimer.ClaimResume(ctx, id)
		if err != nil {
			return nil, err
		}
		if !claimed {
			return nil, errBusySession
		}
	}

	msgs, err := agent.Fork(events, 0)
	if err != nil {
		return nil, fmt.Errorf("rebuild conversation: %w", err)
	}
	mode, ok := requestMode(s.opts.Config.Permissions.Mode, rec.Mode)
	if !ok {
		mode, _ = requestMode(s.opts.Config.Permissions.Mode, "")
	}
	registry, skillReg, _ := s.state.snapshot()
	recorder := agent.NewRecorder(s.store, id, "")
	recorder.Redact = s.opts.Redact
	recorder.Advance(events[len(events)-1].Seq)

	spec := StartSpec{Prompt: rec.Prompt, Mode: mode, User: user, Tenant: tenant}
	if spec.Prompt == "" {
		spec.Prompt = prompt
	}
	live, loop, err := s.buildLive(id, spec, mode, s.opts.Adapter, registry, skillReg, recorder)
	if err != nil {
		return nil, err
	}
	loop.SetHistory(msgs, rec.Turns)
	live.Turns = rec.Turns
	// Idle until the caller's prompt starts it: postMessage treats a running
	// session as one to steer, and there is nothing running yet to steer.
	live.State = "done"

	s.mu.Lock()
	// Re-checked under the lock: a drain that began while the record was
	// being read must not leave a turn running on a node that is exiting.
	if s.draining.Load() {
		s.mu.Unlock()
		return nil, errDraining
	}
	if _, already := s.running[id]; already {
		s.mu.Unlock()
		return nil, errBusySession
	}
	s.running[id] = live
	s.mu.Unlock()
	s.log.Info("session resumed from record", "session", id, "user", user, "events", len(events))
	return live, nil
}

var (
	errNoSession   = errors.New("session not found")
	errBusySession = errors.New("session is already running")
	// errDraining means this node is shutting down. It is not a failure: the
	// caller should be sent to a node that is still accepting work.
	errDraining = errors.New("server is draining")
)

type sessionSummary struct {
	ID      string    `json:"id"`
	User    string    `json:"user"`
	Tenant  string    `json:"tenant"`
	Prompt  string    `json:"prompt"`
	State   string    `json:"state"`
	Created time.Time `json:"created"`
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	tenant := TenantOf(r.Context())
	user := UserOf(r.Context())

	// A durable store also returns sessions from before this process started,
	// which is what makes audit useful after a restart.
	if s.sessions != nil {
		records, err := s.sessions.ListSessions(r.Context(), 200)
		if err == nil {
			out := make([]sessionSummary, 0, len(records))
			for _, rec := range records {
				// The store hands back every session it holds. Row-level
				// security scopes that by TENANT in Postgres, and not at all
				// in the memory store — neither scopes it by user. So the
				// filter has to be here, or one person's list of prompts
				// (which is a list of what they were working on, and often
				// what they uploaded) is shown to everyone else in the tenant.
				if !ownsSession(rec.Tenant, rec.User, tenant, user) {
					continue
				}
				state := "done"
				if rec.EndedAt == nil {
					state = "running"
				}
				s.mu.RLock()
				if live, found := s.running[rec.ID]; found {
					live.mu.Lock()
					state = live.State
					live.mu.Unlock()
				}
				s.mu.RUnlock()
				out = append(out, sessionSummary{
					ID: rec.ID, User: rec.User, Tenant: rec.Tenant,
					Prompt: rec.Prompt, State: state, Created: rec.StartedAt,
				})
			}
			WriteJSON(w, http.StatusOK, out)
			return
		}
		s.log.Warn("durable session list failed, falling back to in-process", "error", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]sessionSummary, 0, len(s.running))
	for _, l := range s.running {
		// Tenant isolation is enforced here AND at the data layer: a boundary
		// that exists in only one place is not a boundary (docs/ops §4). The
		// user check is the same rule s.session() applies to a single session,
		// applied to the list — they must agree, or the list advertises
		// sessions that then 404.
		if !ownsSession(l.Tenant, l.User, tenant, user) {
			continue
		}
		l.mu.Lock()
		out = append(out, sessionSummary{
			ID: l.ID, User: l.User, Tenant: l.Tenant,
			Prompt: l.Prompt, State: l.State, Created: l.Created,
		})
		l.mu.Unlock()
	}
	WriteJSON(w, http.StatusOK, out)
}

// mayAccess reports whether the request's caller owns session id, checking the
// live map first and then the durable store.
//
// The store is consulted because a session outlives the process that ran it:
// after a restart every session is "not running", and refusing those would make
// history unreadable. When neither source knows the id, access is denied —
// failing closed, so an unknown id can never be mistaken for an owned one.
func (s *Server) mayAccess(r *http.Request, id string) bool {
	tenant, user := TenantOf(r.Context()), UserOf(r.Context())
	if _, ok := s.session(id, tenant, user); ok {
		return true
	}
	if s.sessions == nil {
		return false
	}
	records, err := s.sessions.ListSessions(r.Context(), 500)
	if err != nil {
		// A store that cannot answer is not permission to proceed.
		return false
	}
	for _, rec := range records {
		if rec.ID == id {
			return ownsSession(rec.Tenant, rec.User, tenant, user)
		}
	}
	return false
}

// ownsSession reports whether a caller may see a session.
//
// One definition used by both the list and the single-session lookup, because
// the failure this prevents is precisely the two drifting apart: a list that is
// more permissive than the fetch leaks prompts, and one that is stricter hides
// sessions the user can actually open.
//
// The anonymous case is deliberately permissive: with auth disabled every
// caller is "anonymous" in tenant "default", and filtering by user would leave
// a single-user local deployment unable to see its own history.
func ownsSession(recTenant, recUser, tenant, user string) bool {
	if recTenant != tenant {
		return false
	}
	if user == "" || user == "anonymous" {
		return true
	}
	return recUser == user
}

// streamEvents serves the session's event stream over SSE, resumable via
// Last-Event-ID so a dropped connection does not lose the session.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// A session this process is not running may still be replayable from a
	// durable store — that is the whole point of event sourcing. Looking only
	// at the in-memory map meant every session from before a restart returned
	// 404, so clicking one in the UI showed a blank pane.
	live, running := s.session(id, TenantOf(r.Context()), UserOf(r.Context()))
	if !running {
		// Not running is not the same as not ours. Ownership is checked
		// against the stored record before any event is streamed, or a
		// finished session becomes readable by anyone who knows its id.
		if !s.mayAccess(r, id) {
			WriteError(w, http.StatusNotFound, "session not found")
			return
		}
		backlog, err := s.store.Events(id)
		if err != nil || len(backlog) == 0 {
			WriteError(w, http.StatusNotFound, "session not found")
			return
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// ?after= does what Last-Event-ID does for a client that opens a new
	// EventSource, which cannot set the header itself.
	var lastSeq int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		lastSeq, _ = strconv.ParseInt(v, 10, 64)
	} else if v := r.URL.Query().Get("after"); v != "" {
		lastSeq, _ = strconv.ParseInt(v, 10, 64)
	}
	if lastSeq < 0 {
		lastSeq = 0
	}

	// Replay what was missed before subscribing, so no event is dropped in the
	// gap between reconnect and subscription.
	if backlog, err := s.store.Since(id, lastSeq); err == nil {
		for _, ev := range backlog {
			writeSSE(w, ev)
			lastSeq = ev.Seq
		}
		flusher.Flush()
	}

	// Only a session still running in this process can produce new events.
	// For a replayed one the backlog above is the whole story, so close cleanly
	// rather than holding a connection open that will never deliver anything.
	if !running {
		return
	}

	events := s.store.Subscribe(live.ID)
	defer s.store.Unsubscribe(live.ID, events)

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	// The store drops events for a subscriber that falls behind rather than
	// stall the loop. Seeing a full buffer, or a gap in seq, means some may
	// be gone, and they are read back from the record before going on.
	send := func(batch []agent.Event) (ended bool) {
		for _, e := range batch {
			if e.Seq <= lastSeq {
				continue
			}
			lastSeq = e.Seq
			writeSSE(w, e)
			ended = ended || e.Type == agent.EvSessionEnded
		}
		flusher.Flush()
		return ended
	}
	catchUp := func() bool {
		missed, err := s.store.Since(id, lastSeq)
		return err == nil && send(missed)
	}
	behind := false

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-events:
			if !open {
				return
			}
			behind = behind || len(events) >= cap(events)-1
			if ev.Seq > lastSeq+1 {
				behind = true
			}
			if !behind {
				if send([]agent.Event{ev}) {
					return
				}
				continue
			}
			// Drain what is buffered first, then read the rest back once.
			if ev.Seq == lastSeq+1 && send([]agent.Event{ev}) {
				return
			}
			if len(events) > 0 {
				continue
			}
			behind = false
			if catchUp() {
				return
			}
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// replaySession returns the full event list. Deterministic replay is what makes
// audit and incident reconstruction work (docs P6).
func (s *Server) replaySession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// A durable store can replay a session this process never ran, so a miss in
	// the live map is not by itself a 404. Ownership still has to be proven
	// against the stored record: row-level security scopes the query by tenant,
	// never by user, so without this any signed-in account could replay a
	// colleague's full transcript by id.
	if !s.mayAccess(r, id) {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	events, err := s.store.Events(id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(events) == 0 {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	WriteJSON(w, http.StatusOK, events)
}

// hawkeyeSession reports on a session: JSON by default, the full page with
// ?format=html. It reads the same record replay does, so it answers to the
// same ownership check — a report carries every tool result in the session.
func (s *Server) hawkeyeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.mayAccess(r, id) {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	events, err := s.store.Events(id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(events) == 0 {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	rep := hawkeye.Analyze(id, events)
	if r.URL.Query().Get("format") != "html" {
		WriteJSON(w, http.StatusOK, rep)
		return
	}
	page, err := hawkeye.HTML(rep)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not render the report")
		return
	}
	// The page embeds untrusted tool output. It is escaped, and this makes
	// sure that stays true even if a future template change gets it wrong.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

// postMessage continues an existing conversation.
//
// This is what makes the console a chat rather than a series of one-shots: the
// Loop is retained per session, so a follow-up resolves against everything that
// came before — including which files have been read, which is what lets the
// second message edit what the first one looked at.
func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		WriteError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if !validClientID(req.ClientID) {
		WriteError(w, http.StatusBadRequest, "client_id is up to 64 letters, digits, - and _")
		return
	}
	msg := agent.Message{Text: req.Prompt, ClientID: req.ClientID}

	live, ok := s.session(id, TenantOf(r.Context()), UserOf(r.Context()))
	if !ok {
		// Not running here is not the end of it. A finished session is
		// continued from its record — by this process after a restart, or by
		// another node entirely — provided the caller owns it.
		if !validSessionID(id) || !s.mayAccess(r, id) {
			WriteError(w, http.StatusNotFound, "session not found")
			return
		}
		resumed, err := s.resumeSession(r.Context(), id, req.Prompt, UserOf(r.Context()), TenantOf(r.Context()))
		switch {
		case errors.Is(err, errNoSession):
			WriteError(w, http.StatusNotFound, "session not found")
			return
		case errors.Is(err, errBusySession):
			WriteError(w, http.StatusConflict, "session is being continued elsewhere")
			return
		case errors.Is(err, errDraining):
			// 503 with Retry-After is what a balancer reads as "take me out
			// of rotation", rather than as an error to show the user.
			w.Header().Set("Retry-After", "5")
			WriteError(w, http.StatusServiceUnavailable, "server is shutting down; retry")
			return
		case err != nil:
			s.log.Error("resume failed", "session", id, "error", err)
			WriteError(w, http.StatusInternalServerError, "could not continue the session")
			return
		}
		live = resumed
	}

	live.mu.Lock()
	busy := live.State == "running" || live.State == "waiting_approval"
	if busy && !req.Interrupt {
		// A message to a working agent steers it rather than being refused.
		// Interrupting and re-asking throws away everything the run has
		// already established — the files read, the tool results, the context
		// built — and makes the user pay for it twice. The message is applied
		// at the next turn boundary, so a call in flight still completes and
		// the transcript never shows one with no result. Queued under the
		// lock, so a run cannot decide to end between the check and the queue.
		qid := live.Loop.QueueMessage(msg)
		live.mu.Unlock()
		WriteJSON(w, http.StatusAccepted, map[string]string{
			"session_id": id,
			"delivery":   "steered",
			"queue_id":   qid,
		})
		return
	}
	if busy {
		// Send now: stop the running turn, wait for the loop to be free, and
		// start this one fresh.
		stop, ran := live.cancel, live.ran
		live.mu.Unlock()
		if stop != nil {
			stop()
		}
		if ran != nil {
			select {
			case <-ran:
			case <-time.After(30 * time.Second):
				WriteError(w, http.StatusConflict, "the running turn did not stop in time")
				return
			case <-r.Context().Done():
				return
			}
		}
		live.mu.Lock()
		if live.State != "done" {
			live.mu.Unlock()
			WriteError(w, http.StatusConflict, "another turn started first")
			return
		}
	}
	live.State = "running"
	live.Turns++
	live.ran = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	live.cancel = cancel
	live.mu.Unlock()

	go func() {
		defer cancel()
		live.undo.BeginTurn()
		reason, err := live.Loop.RunMessage(ctx, msg)
		for live.settle(ctx, reason, err) {
			reason, err = live.Loop.RunQueued(ctx)
		}
		if err != nil {
			s.log.Error("follow-up failed", "session", id, "error", err)
			return
		}
		s.log.Info("follow-up ended", "session", id, "reason", reason)
	}()

	WriteJSON(w, http.StatusAccepted, map[string]string{"session_id": id})
}

// settle ends a run, unless a message was queued after the loop last looked
// and the run ended cleanly: then it reports true and the caller runs again.
func (l *liveSession) settle(ctx context.Context, reason agent.TerminalReason, err error) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil && ctx.Err() == nil && reason == agent.TermCompleted && len(l.Loop.Queued()) > 0 {
		return true
	}
	l.State = "done"
	if l.ran != nil {
		close(l.ran)
		l.ran = nil
	}
	return false
}

// notRunningHere answers for a session this process is not running: 421 with
// the node that holds it, as approveAction does, or 404.
func (s *Server) notRunningHere(w http.ResponseWriter, r *http.Request, id string) {
	if node := s.elsewhere(r.Context(), id); node != "" {
		w.Header().Set("Abhed-Session-Node", node)
		WriteError(w, http.StatusMisdirectedRequest, "this session is running on another node; route by session id")
		return
	}
	WriteError(w, http.StatusNotFound, "session not found")
}

// listQueue returns the messages waiting for the session's next turn boundary.
func (s *Server) listQueue(w http.ResponseWriter, r *http.Request) {
	live, ok := s.session(r.PathValue("id"), TenantOf(r.Context()), UserOf(r.Context()))
	if !ok {
		s.notRunningHere(w, r, r.PathValue("id"))
		return
	}
	q := live.Loop.Queued()
	if q == nil {
		q = []agent.QueuedMessage{}
	}
	WriteJSON(w, http.StatusOK, q)
}

// cancelQueued withdraws a queued message before the loop reads it. Once it
// has been delivered it cannot be withdrawn, and the answer is 404.
func (s *Server) cancelQueued(w http.ResponseWriter, r *http.Request) {
	live, ok := s.session(r.PathValue("id"), TenantOf(r.Context()), UserOf(r.Context()))
	if !ok {
		s.notRunningHere(w, r, r.PathValue("id"))
		return
	}
	live.mu.Lock()
	removed := live.Loop.Unqueue(r.PathValue("qid"))
	live.mu.Unlock()
	if !removed {
		WriteError(w, http.StatusNotFound, "no such queued message; it may already have been delivered")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) interruptSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	live, ok := s.session(id, TenantOf(r.Context()), UserOf(r.Context()))
	if !ok {
		s.notRunningHere(w, r, r.PathValue("id"))
		return
	}
	live.mu.Lock()
	c := live.cancel
	live.mu.Unlock()
	if c != nil {
		c()
	}
	live.Cancel()
	w.WriteHeader(http.StatusNoContent)
}

type approveRequest struct {
	Approved bool   `json:"approved"`
	Scope    string `json:"scope,omitempty"`
}

func (s *Server) approveAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	live, ok := s.session(id, TenantOf(r.Context()), UserOf(r.Context()))
	if !ok {
		// The session is not here. If the store holds the approval, answer it
		// anyway: the waiting node polls for the result, so the reviewer's
		// decision still arrives. This is the case sticky routing exists to
		// avoid and the one durability exists to survive when it does not.
		if s.answerElsewhere(w, r, id) {
			return
		}
		if node := s.elsewhere(r.Context(), id); node != "" {
			w.Header().Set("Abhed-Session-Node", node)
			WriteError(w, http.StatusMisdirectedRequest,
				"this session is running on another node; route by session id")
			return
		}
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	var req approveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Record the decision where the waiting node can see it, whichever node
	// that is. Harmless when it is this one: the channel below answers first
	// and the second write finds the row already answered.
	if d := s.approvalStore(); d != nil {
		if pending, found, err := d.PendingApproval(r.Context(), id); err == nil && found {
			_, _ = d.AnswerApproval(r.Context(), pending.ID, req.Approved, UserOf(r.Context()))
		}
	}

	select {
	case live.approvals <- approvalReply(req):
		w.WriteHeader(http.StatusNoContent)
	default:
		WriteError(w, http.StatusConflict, "no approval is pending for this session")
	}
}

// LocalAuth returns the local-account provider, or nil when this deployment
// does not hold passwords itself. Exported because ending someone's access
// means ending the sessions they already have, and only this type can do
// that; the handler that decides to is not necessarily in this package.
func (s *Server) LocalAuth() *auth.LocalAuth {
	for _, p := range s.signIns() {
		if l, ok := p.(*auth.LocalAuth); ok {
			return l
		}
	}
	return nil
}

// redirectHome sends /login to the front door, which is where the sign-in form
// lives when Abhed holds the accounts.
func (s *Server) redirectHome(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/", http.StatusFound)
}

// signOut clears whichever session the browser is holding. With more than
// one provider we cannot know which signed this user in, and clearing the
// wrong one leaves them still signed in after clicking Sign out. With none
// holding a session the first provider still gets the request: an identity
// provider may hold its own session that needs ending too.
func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	providers := s.signIns()
	for _, p := range providers {
		if _, found := p.Identify(r); found {
			p.SignOut(w, r)
			return
		}
	}
	providers[0].SignOut(w, r)
}

// whoami answers for whichever session exists, and says which mechanism
// holds it, so the console can show the right chip.
func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	for _, p := range s.signIns() {
		if id, found := p.Identify(r); found {
			me := map[string]any{
				"authenticated": true, "auth_mode": p.Name(),
				"subject": id.Subject, "email": id.Email, "name": id.Name,
				"tenant": id.Tenant, "groups": id.Groups,
			}
			// Switching user is a fresh sign-in: the identity provider's own
			// prompt when there is one, the sign-in page for local accounts.
			switch p.Name() {
			case "local":
				me["switch_url"], me["password_url"] = "/logout", "/account"
			case "oidc":
				me["switch_url"] = "/switch-user"
			}
			WriteJSON(w, http.StatusOK, me)
			return
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"authenticated": false,
		"auth_mode":     orDefaultStr(s.opts.Config.Auth.Mode, "none"),
	})
}

// signup creates an account from the sign-in page. Off unless a deployment
// explicitly opts in: on an internal tool, open registration is a way in for
// anyone who can reach the port, not a convenience.
func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	local := s.LocalAuth()
	if local == nil {
		WriteJSON(w, http.StatusForbidden, map[string]string{
			"error": "self-registration is disabled"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
		Invite   string `json:"invite,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid request"})
		return
	}
	// Three admission modes. Open registration on a public URL hands a
	// stranger an agent with a shell, so it stays off by default — but
	// "closed" was blocking adoption, and an invite is the middle ground:
	// the operator decides who gets in without having to create every
	// account by hand. Invites are an edition's to issue; without a redeemer
	// the third mode does not exist and closed means closed.
	//
	// A new account gets NO groups, so an invited user is an ordinary user
	// until an administrator promotes them.
	if !s.opts.Config.Auth.AllowSignup {
		if s.opts.Invites == nil {
			WriteJSON(w, http.StatusForbidden, map[string]string{
				"error": "self-registration is disabled on this server; " +
					"ask the operator for an account"})
			return
		}
		if strings.TrimSpace(req.Invite) == "" {
			WriteJSON(w, http.StatusForbidden, map[string]string{
				"error": "an invite code is required to register here"})
			return
		}
		if err := s.opts.Invites.Redeem(r.Context(), req.Invite, req.Username); err != nil {
			WriteJSON(w, http.StatusForbidden, map[string]string{
				"error": err.Error()})
			return
		}
	}

	u := auth.User{
		Username: req.Username, Email: req.Email, Name: req.Name,
		Tenant: orDefaultStr(s.opts.Config.Auth.DefaultTenant, "default"),
	}
	if err := local.CreateUser(r.Context(), u, req.Password); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Link the account to the code it came from, after creation rather than
	// before: a failure here must not leave a record claiming an account that
	// does not exist.
	if s.opts.Invites != nil && req.Invite != "" {
		if err := s.opts.Invites.Redeemed(r.Context(), req.Invite, req.Username); err != nil {
			s.log.Error("link account to invite", "user", req.Username, "err", err)
		}
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// whoamiDisabled reports that this deployment runs without authentication.
// The console uses it to decide whether to show a user chip at all.
func (s *Server) whoamiDisabled(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{
		"authenticated": false,
		"auth_mode":     orDefaultStr(s.opts.Config.Auth.Mode, "none"),
		"reason":        "authentication is not configured on this server",
	})
}

// authDisabledPage explains why /login and /logout do nothing here, and how to
// turn them on. A bare 404 reads as a bug; this reads as a setting.
func (s *Server) authDisabledPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(authDisabledHTML))
}

// serveLanding is the front door.
//
// Previously / went straight into the workspace, which told a first-time
// visitor nothing about what Abhed is and gave a configured deployment no
// place to sign in. It now shows what this instance actually is — model,
// sandbox tier, storage, auth mode, live session counts — and routes on:
// straight through when there is nothing to sign in to, or to the IdP when
// there is.
func (s *Server) serveLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.Write([]byte(WithHome(landingHTML, s.opts.HomeURL)))
}

// overviewResponse is what the landing page renders. Everything here is a fact
// about THIS deployment, so the page describes the instance in front of you
// rather than a product in the abstract.
type overviewResponse struct {
	Model         string `json:"model"`
	ContextWindow int    `json:"context_window"`
	// Workspace is empty for anonymous callers: it is an absolute host path,
	// so it names the operator's account and directory layout.
	Workspace  string `json:"workspace,omitempty"`
	Sandbox    string `json:"sandbox"`
	SandboxNet bool   `json:"sandbox_network"`
	// Isolation describes the boundary in the terms that actually apply to this
	// deployment. A containerised Abhed reports sandbox tier "none" — correct,
	// because the boundary is the container around the whole process rather
	// than a sandbox inside it — and presenting that bare number as a warning
	// would tell the reader the opposite of the truth.
	Isolation string `json:"isolation,omitempty"`
	// IsolationOK is whether the deployment is actually contained, as opposed
	// to whether a particular tier string was configured.
	IsolationOK   bool   `json:"isolation_ok"`
	Storage       string `json:"storage"`
	Durable       bool   `json:"durable"`
	AuthMode      string `json:"auth_mode"`
	SignInURL     string `json:"sign_in_url,omitempty"`
	Authenticated bool   `json:"authenticated"`
	// LocalAuth tells the landing page to render a username/password form
	// rather than a redirect button.
	LocalAuth   bool `json:"local_auth"`
	AllowSignup bool `json:"allow_signup"`
	// InviteSignup reports that registration is possible with a code.
	//
	// Distinct from AllowSignup, which means "anyone may register". The two
	// booleans together are what let the page offer a code field instead of
	// either an open form or nothing at all — with only AllowSignup, an
	// invite-only deployment is indistinguishable from a closed one and the
	// UI correctly hides a door that is in fact open.
	InviteSignup  bool   `json:"invite_signup"`
	ProviderLabel string `json:"provider_label,omitempty"`
	// AdminURL is where the Admin link goes, when an edition has mounted a
	// page there. Empty means no page: the link is not drawn, whoever is
	// looking. Admin below still says whether the caller may administer,
	// because settings and users are served regardless of any page.
	AdminURL string `json:"admin_url,omitempty"`
	User     string `json:"user,omitempty"`
	Tenant   string `json:"tenant,omitempty"`
	// Admin is whether the signed-in identity is in the admin group. It
	// exists so the UI can show the way to /admin to the people who can use
	// it. It is NOT what protects /admin: every admin route is wrapped in
	// s.Admin() on the server, so a client that flips this in the browser
	// gets a link to a page that answers 403. Hiding a control is courtesy;
	// the guard is the boundary.
	Admin      bool     `json:"admin"`
	WebSearch  string   `json:"web_search"`
	Retrieval  bool     `json:"retrieval"`
	MCPServers int      `json:"mcp_servers"`
	Tools      []string `json:"tools"`
	Sessions   int      `json:"sessions"`
	Events     int64    `json:"events"`
	Running    int      `json:"running"`
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	cfg := s.opts.Config
	o := overviewResponse{
		Model:         s.opts.Adapter.Profile().Name,
		ContextWindow: s.opts.Adapter.Profile().ContextWindow,
		AuthMode:      orDefaultStr(cfg.Auth.Mode, "none"),
		Durable:       cfg.Storage.Driver == "postgres",
		Retrieval:     cfg.Retrieval.Enabled,
		MCPServers:    len(cfg.MCP.Servers),
	}

	o.Storage = "in-memory (sessions end with this process)"
	if o.Durable {
		o.Storage = "postgres · sessions survive restart"
	}

	o.Sandbox = orDefaultStr(cfg.Sandbox.MinTier, "process")
	o.SandboxNet = cfg.Sandbox.AllowNetwork

	// ABHED_IN_CONTAINER is set by the deployment image, so this reports how
	// the process is actually running rather than what a config file claims.
	// The distinction matters: inside a container, tier "none" is the correct
	// setting and the strongest available posture, because the boundary is the
	// container itself.
	if os.Getenv("ABHED_IN_CONTAINER") != "" {
		o.Isolation = "container"
		o.IsolationOK = true
	} else {
		o.Isolation = o.Sandbox
		o.IsolationOK = o.Sandbox != "none"
	}

	o.WebSearch = "disabled"
	if cfg.WebSearch.Enabled {
		o.WebSearch = orDefaultStr(cfg.WebSearch.Provider, "duckduckgo")
	}

	if reg := s.state.toolRegistry(); reg != nil {
		o.Tools = reg.Names()
	}

	// Sign-in only matters when there is somewhere to sign in TO.
	if local := s.LocalAuth(); local != nil {
		o.LocalAuth = true
		o.AllowSignup = cfg.Auth.AllowSignup
		o.InviteSignup = !cfg.Auth.AllowSignup && s.opts.Invites != nil
	}
	o.AdminURL = s.opts.AdminURL
	for _, p := range s.signIns() {
		if u, label := p.SignIn(); u != "" {
			o.SignInURL = u + "?return=%2Fconsole"
			o.ProviderLabel = label
		}
		if id, found := p.Identify(r); found {
			o.Authenticated = true
			o.User = orDefaultStr(id.Email, id.Subject)
			o.Admin = slices.Contains(id.Groups, s.adminGroup())
			o.Tenant = id.Tenant
		}
	}

	// The workspace is an absolute path on the host: it names the operator's
	// account and directory layout, which is reconnaissance for anyone probing
	// the box. This endpoint is public so the landing page can describe the
	// deployment honestly before sign-in, and everything above is a property of
	// the deployment rather than of the machine. The path is not, so it is
	// disclosed only to someone who has already authenticated.
	if o.Authenticated {
		o.Workspace = s.opts.Workspace
	}

	s.mu.RLock()
	for _, l := range s.running {
		l.mu.Lock()
		if l.State == "running" || l.State == "waiting_approval" {
			o.Running++
		}
		l.mu.Unlock()
	}
	s.mu.RUnlock()

	if s.sessions != nil {
		if recs, err := s.sessions.ListSessions(r.Context(), 500); err == nil {
			o.Sessions = len(recs)
		}
	}
	if pg, ok := s.store.(interface {
		Stats(context.Context) (int64, int64, error)
	}); ok {
		if _, events, err := pg.Stats(r.Context()); err == nil {
			o.Events = events
		}
	}

	WriteJSON(w, http.StatusOK, o)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	n := len(s.running)
	s.mu.RUnlock()
	WriteJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"sessions": n,
		"model":    s.opts.Adapter.Profile().Name,
	})
}

// session resolves a live session for a caller, or reports absence.
//
// Both the tenant AND the user must match. Tenancy alone was the original
// check, which quietly meant every user in a tenant could read another user's
// transcript, post to their agent, interrupt it, and — worst of all — answer
// its approval prompts. Approving a dangerous tool call on someone else's
// behalf is a privilege the model was never meant to accept from a bystander.
//
// A caller who is not the owner gets the same "not found" as a caller who
// invented the ID, so the lookup does not confirm that a session exists.
// claimNode records that this process holds the session, when the deployment
// is configured for several. A failure is logged and ignored: routing is an
// optimisation, and refusing to start a turn because a bookkeeping write
// failed would be a worse outcome than a misrouted request.
func (s *Server) claimNode(ctx context.Context, sessionID string) {
	r, ok := s.router()
	if !ok {
		return
	}
	if err := r.ClaimNode(ctx, sessionID, s.opts.NodeID); err != nil {
		s.log.Warn("could not claim session for this node", "session", sessionID, "err", err)
	}
}

// heartbeatNode keeps this node's claim on the session fresh while the turn
// runs. Without it a claim ages out of the staleness window and a healthy node
// stops being found, so the turns most likely to need an approval — the long
// ones — are exactly the ones whose approvals get misrouted.
//
// The returned function stops the heartbeat; it is safe to call more than once.
func (s *Server) heartbeatNode(ctx context.Context, sessionID string) func() {
	return s.heartbeatNodeEvery(ctx, sessionID, nodeHeartbeat)
}

func (s *Server) heartbeatNodeEvery(ctx context.Context, sessionID string, every time.Duration) func() {
	if _, ok := s.router(); !ok {
		return func() {}
	}
	ctx, stop := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Its own context: the run's may be seconds from cancellation,
				// and a refresh that fails then would look like a dead node.
				beat, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				s.claimNode(beat, sessionID)
				cancel()
			}
		}
	}()
	return stop
}

// releaseNode clears the claim when a turn finishes. It uses its own context:
// the run's context is cancelled by the time this is reached.
func (s *Server) releaseNode(sessionID string) {
	r, ok := s.router()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.ReleaseNode(ctx, sessionID, s.opts.NodeID); err != nil {
		s.log.Warn("could not release session claim", "session", sessionID, "err", err)
	}
}

// answerElsewhere records a decision for a session this node is not running,
// and reports whether it did. The node that is waiting polls for the result,
// so the answer is delivered without the request ever reaching it.
func (s *Server) answerElsewhere(w http.ResponseWriter, r *http.Request, sessionID string) bool {
	d := s.approvalStore()
	if d == nil {
		return false
	}
	pending, found, err := d.PendingApproval(r.Context(), sessionID)
	if err != nil || !found {
		return false
	}
	var req approveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return true
	}
	answered, err := d.AnswerApproval(r.Context(), pending.ID, req.Approved, UserOf(r.Context()))
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not record the decision")
		return true
	}
	if !answered {
		WriteError(w, http.StatusConflict, "this approval was already answered")
		return true
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}

// approvalStore reports the store when it can hold approvals durably, and
// nil when it cannot. Unlike routing this does not need a node id: recording
// the exchange is worth doing on one node too, because it makes a pending
// approval visible in the record rather than only in memory.
func (s *Server) approvalStore() ApprovalStore {
	a, ok := s.store.(ApprovalStore)
	if !ok {
		return nil
	}
	return a
}

// router reports the routing store, and whether routing applies at all.
// Both a node identity and a store that can record one are required.
func (s *Server) router() (SessionRouter, bool) {
	if s.opts.NodeID == "" {
		return nil, false
	}
	r, ok := s.store.(SessionRouter)
	return r, ok
}

func (s *Server) session(id, tenant, user string) (*liveSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	live, found := s.running[id]
	if !found || !ownsSession(live.Tenant, live.User, tenant, user) {
		return nil, false
	}
	return live, true
}

// elsewhere reports the node holding a session that this process does not,
// so a caller can be told where to go rather than told it does not exist.
//
// Returns "" when routing is off, when no node holds the session, or when
// this node is the holder — all of which mean "the ordinary not-found answer
// is the right one".
func (s *Server) elsewhere(ctx context.Context, sessionID string) string {
	r, ok := s.router()
	if !ok {
		return ""
	}
	node, err := r.NodeFor(ctx, sessionID, nodeStale)
	if err != nil || node == "" || node == s.opts.NodeID {
		return ""
	}
	return node
}

// Approve implements agent.Approver for a server session: it publishes the
// pending request and blocks until a reviewer answers or the session is
// cancelled. This is what enables headless runs with a human gate.
func (l *liveSession) Approve(ctx context.Context, tool string, args json.RawMessage, res policy.Result) (bool, error) {
	// Already allowed for this session: a reviewer chose "always allow" for
	// this scope earlier, so proceed without asking again.
	if res.Scope != "" {
		l.mu.Lock()
		remembered := l.allowed[res.Scope]
		l.mu.Unlock()
		if remembered {
			return true, nil
		}
	}

	l.mu.Lock()
	l.State = "waiting_approval"
	l.pending = &pendingApproval{Tool: tool, Args: args, Reason: res.Reason, Scope: res.Scope}
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		l.State = "running"
		l.pending = nil
		l.mu.Unlock()
	}()

	// Record it durably where the store can. The answer may arrive at
	// another node, and a channel in this process is not reachable from
	// there. A failure to record is not a reason to refuse the turn: the
	// in-memory path below still works for an answer that lands here.
	var durableID string
	if l.durable != nil {
		id, err := l.durable.AskApproval(ctx, store.Approval{
			SessionID: l.ID, Tool: tool, Args: args,
			Reason: res.Reason, Scope: res.Scope,
		})
		if err == nil {
			durableID = id
		}
	}

	// Poll only when there is something to poll for.
	var poll <-chan time.Time
	if durableID != "" {
		t := time.NewTicker(approvalPoll)
		defer t.Stop()
		poll = t.C
	}

	// Started once, outside the loop: time.After inside it would restart the
	// deadline on every poll tick and never fire.
	deadline := time.NewTimer(30 * time.Minute)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()

		case <-poll:
			approved, answered, err := l.durable.ApprovalResult(ctx, durableID)
			if err != nil || !answered {
				continue
			}
			if approved && res.Scope != "" {
				l.mu.Lock()
				l.allowed[res.Scope] = true
				l.mu.Unlock()
			}
			return approved, nil

		case reply := <-l.approvals:
			// "Always allow" carries the scope back; remember it so the next call
			// matching the same rule is not re-prompted.
			if reply.Approved && reply.Scope != "" {
				l.mu.Lock()
				l.allowed[reply.Scope] = true
				l.mu.Unlock()
			}
			return reply.Approved, nil

		case <-deadline.C:
			// Fail closed: an unanswered approval must not become an approval.
			return false, nil
		}
	}
}

// storeTenant reconciles the request's tenant with the store's configured one.
// When authentication is off there is no meaningful per-request tenant, so the
// configured one wins; with real auth the token's tenant is authoritative.
func storeTenant(cfg config.Config, requestTenant string) string {
	if cfg.Auth.Mode == "none" || cfg.Auth.Mode == "" {
		if cfg.Storage.Tenant != "" {
			return cfg.Storage.Tenant
		}
	}
	return requestTenant
}

func orDefaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func writeSSE(w http.ResponseWriter, ev agent.Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	// No "event:" field, deliberately. Naming an SSE event makes the browser
	// dispatch it to addEventListener(name), and EventSource.onmessage then
	// never fires — which silently produced an empty transcript in the console
	// while curl, which ignores the field, showed the data arriving fine.
	// The type is already in the JSON payload, so one generic handler is both
	// correct and simpler than registering a listener per event type.
	fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, data)
}

// WriteJSON writes a JSON response the way every built-in handler does, so a
// mounted one answers in the same shape.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the status is already on the wire; there is no second answer to give
}

// WriteError writes the {"error": msg} shape the console expects.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// A slow body is the other half of slowloris: headers arrive promptly,
		// then the body trickles in a byte at a time and holds the connection
		// open. Generous enough for a large upload on a poor connection.
		ReadTimeout: 5 * time.Minute,
		// Idle keep-alive connections cost a goroutine each; an attacker opening
		// thousands and sending nothing is otherwise free.
		IdleTimeout: 2 * time.Minute,
		// 1 MB of headers is the Go default and far more than anything here
		// sends; stating it makes the bound deliberate rather than inherited.
		MaxHeaderBytes: 1 << 20,
		// No write timeout: SSE streams are long-lived by design.
	}
	go func() {
		<-ctx.Done()
		s.drain()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			s.log.Warn("shutdown did not complete cleanly", "err", err)
		}
	}()
	s.log.Info("abhed server listening", "addr", s.opts.Addr, "workspace", s.opts.Workspace)
	return srv.ListenAndServe()
}

// drain stops this node taking new turns and gives the running ones until
// DrainTimeout to finish. What is still running when the budget runs out is
// cancelled with a stated cause, so it records as a shutdown rather than as a
// user interrupt and can be resumed on another node.
//
// With no budget configured this is the old behaviour: end everything at once.
func (s *Server) drain() {
	s.draining.Store(true)

	if s.opts.DrainTimeout > 0 {
		deadline := time.NewTimer(s.opts.DrainTimeout)
		defer deadline.Stop()
		poll := time.NewTicker(100 * time.Millisecond)
		defer poll.Stop()

		if n := s.runningCount(); n > 0 {
			s.log.Info("draining", "turns", n, "budget", s.opts.DrainTimeout)
		}
		for s.runningCount() > 0 {
			select {
			case <-deadline.C:
				s.log.Warn("drain budget spent, ending turns still running",
					"turns", s.runningCount())
				s.cancelRunning()
				return
			case <-poll.C:
			}
		}
		s.log.Info("drained with no turns left running")
		return
	}
	s.cancelRunning()
}

func (s *Server) runningCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, live := range s.running {
		live.mu.Lock()
		state := live.State
		live.mu.Unlock()
		// A session sitting at "done" is resumable but not working; it holds
		// no turn, so waiting on it would spend the whole budget for nothing.
		if state != "done" {
			n++
		}
	}
	return n
}

func (s *Server) cancelRunning() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, live := range s.running {
		if live.cancelCause != nil {
			live.cancelCause(agent.ErrShutdown)
		}
	}
}

// tapStore forwards to the real store and hands each appended event to a
// function that must not block. Only Append is intercepted; reads and
// subscriptions go straight through.
type tapStore struct {
	EventStore
	tap func(agent.Event)
}

func (t tapStore) Append(ev agent.Event) error {
	err := t.EventStore.Append(ev)
	if err == nil {
		t.tap(ev)
	}
	return err
}
