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
	"net/url"
	"os"
	"reflect"
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
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/index"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/skills"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/webfetch"
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

// OrphanClaimer is implemented by stores that can take over a session a
// crashed process left open: still open, and its holder's liveness older
// than stale (or never written). The claim writes holder as the session's
// holder in the same conditional update, so two processes cannot both take one.
type OrphanClaimer interface {
	ClaimOrphan(ctx context.Context, sessionID, holder string, stale time.Duration) (bool, error)
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

// LeaseRenewer is implemented by stores whose heartbeat is fenced: a renewal
// succeeds only while the session is still this holder's, and false says
// another process has taken it over.
type LeaseRenewer interface {
	RenewNode(ctx context.Context, sessionID, holder string) (bool, error)
}

// errHoldFailed is a hold on a session this process could not record; every
// path answers it 503 with Retry-After, as a store that is briefly away.
var errHoldFailed = errors.New("could not hold the session")

// writeHoldFailed answers a request whose session could not be held.
func writeHoldFailed(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	WriteError(w, http.StatusServiceUnavailable, "could not hold the session; retry")
}

// errLeaseLost refuses every write for a session this process no longer holds.
var errLeaseLost = errors.New("this process no longer holds the session: another has taken it over")

// ApprovalStore is implemented by stores that can hold a pending approval
// durably, so a reviewer's answer reaches the waiting turn from any node.
//
// Without it the exchange stays in memory, which is correct for one server
// and loses the answer behind a load balancer.
type ApprovalStore interface {
	AskApproval(ctx context.Context, a store.Approval) (string, error)
	// AnswerApproval records a decision and the "always allow" scope it
	// carried, empty for this call only; it refuses an answered or ended row.
	AnswerApproval(ctx context.Context, id string, approved bool, scope, by string) (bool, error)
	ApprovalResult(ctx context.Context, id string) (approved, answered bool, scope string, err error)
	// EndApproval closes the row once its request stops waiting.
	EndApproval(ctx context.Context, id string) error
	PendingApproval(ctx context.Context, sessionID string) (store.Approval, bool, error)
}

// approvalPoll is how often a waiting turn checks the database for an answer.
// Short enough that a reviewer does not notice the delay, long enough that a
// thirty-minute wait is not thousands of queries.
const approvalPoll = 2 * time.Second

// nodeStale is how long a claim survives without being refreshed. Longer than
// any turn boundary, short enough that a node which died does not strand its
// sessions for long. The store's own claims use the same window.
var nodeStale = store.HolderStale

// nodeHeartbeat refreshes the claim well inside nodeStale, so a slow write or
// a missed tick does not make a healthy node look dead.
var nodeHeartbeat = 30 * time.Second

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

// InviteEmailChecker is an InviteRedeemer whose codes may be made out to one
// address. Signup asks it before the code is spent, so a refusal keeps the code.
type InviteEmailChecker interface {
	CheckInviteEmail(ctx context.Context, code, email string) error
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
	// It holds no '#', which separates it from a lease's incarnation token.
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
	// Redact rewrites every event payload before it is written. Nil, a typed nil
	// included, means the operator's secrets store, read again for each session.
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
	// OwnerActive says whether a session's owner may still act: before an
	// automatic wake run starts on their behalf, and again inside it before
	// each model call and approval. Nil uses the local accounts when there
	// are any, and assumes active otherwise. An edition supplies its own to
	// cover revoked, disabled or departed users; it must answer false when
	// it cannot tell.
	OwnerActive func(ctx context.Context, tenant, user string) bool
	// Agents are the subagent types sessions offer: the built-in roles and
	// the loaded definitions. Nil offers the built-in roles only, until an
	// admin reload reads the definitions.
	Agents *agent.Definitions
	// SkillRegistry is the loaded skill set. Held alongside SkillListing so a
	// settings change can re-render the listing rather than being stuck with
	// the string computed at startup.
	SkillRegistry *skills.Registry
	// Gateway holds the MCP connections, so a server can be added at runtime.
	Gateway *mcp.Gateway
	// Extensions are the running extensions: each session's policy carries
	// their veto, and compaction asks them for a summary. Nil runs none.
	Extensions *extension.Host
	// Index backs the retrieval tool, for a reindex triggered from settings.
	Index        *index.Index
	IndexOptions index.BuildOptions
	// AdminAudit, when set, is told of every administrative change made
	// through /v1/admin/*; nil means the server log only. It may run under the
	// admin-rights lock, so it must not call an admin route itself.
	AdminAudit func(ctx context.Context, action, target string, detail map[string]any)
	// DrainTimeout is how long a shutdown waits for running turns to finish
	// before cancelling them. Zero keeps the old behaviour of ending them at
	// once, which is what a single-node deployment with no balancer wants.
	// A shutdown takes at most DrainTimeout + turnEndWait (5s) + 10s for HTTP,
	// which must fit the process's grace period (30s by default on Kubernetes).
	DrainTimeout time.Duration
	// StreamRecheck is how often an open event or terminal stream is
	// authorised again. Zero means the default; it can only be shortened, and
	// anything above maxStreamRecheck is held to it.
	StreamRecheck time.Duration
}

// Server holds live sessions and serves the API.
type Server struct {
	opts Options
	// searching counts the workspace searches running per session.
	searching sync.Map
	store     EventStore
	sessions  SessionRecorder // nil when the store is not durable
	log       *slog.Logger
	mu        sync.RWMutex
	running   map[string]*liveSession
	// starting holds sessions whose row is written but which are not yet in
	// running; the list leaves them out, since their routes would answer 404.
	starting map[string]bool
	// draining is set once shutdown starts: running turns finish, new ones
	// are refused so a balancer sends them to a node that can take them.
	draining atomic.Bool
	// holder is this process's liveness identity: NodeID when set, otherwise
	// an id of its own for its lifetime. Every session it holds is claimed
	// and heartbeated under it, so "no heartbeat" never reads as "dead".
	holder string

	// Throttles for the endpoints reachable before authentication succeeds.
	signinLimiter  *limiter
	sessionLimiter *limiter

	// mounts added after construction, ahead of the ones in Options.
	mounts []Mount

	// adminMu serialises admin-rights changes, so two demotions at once
	// cannot leave nobody an administrator. It holds within one process only.
	adminMu sync.Mutex

	// state holds what a settings change may replace, behind its own lock.
	// Separate from opts, which stays immutable — mixing "set once" and
	// "changes at runtime" in one struct is how a field ends up read without
	// the lock.
	state *mutable

	// streams are the long-lived responses open now, told to authorise again
	// when a sign-in changes (stream_auth.go).
	streamMu sync.Mutex
	streams  map[*streamGuard]struct{}
}

type liveSession struct {
	ID      string
	User    string
	Tenant  string
	Loop    *agent.Loop
	Cancel  context.CancelFunc
	Created time.Time
	Prompt  string
	State   string // running | waiting_approval | background | idle | done
	// title is the name a person last gave the session; guarded by mu.
	title string
	// Reason is how the last run ended, which the session list shows for done.
	Reason agent.TerminalReason
	Turns  int // exchanges in this conversation
	cancel context.CancelFunc
	// ran is closed when the current run's goroutine ends, so a caller that
	// interrupted it can wait for the loop to be free.
	ran chan struct{}
	// cancelCause ends the run with a stated reason, so shutdown is not
	// recorded as a user interrupt.
	cancelCause context.CancelCauseFunc
	// pending is the approval request the turn is waiting on (approval.go).
	pending *pendingApproval
	// ended holds the last endedKept requests that stopped waiting, oldest
	// dropped first, so a late answer to one is refused at once.
	ended      map[string]bool
	endedOrder []string
	// parked counts answers waiting for their request, bounded by maxParked.
	parked int
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
	// lineAsks is when each destructive terminal line was last asked about, under manualMu.
	lineAsks map[string]time.Time
	// shellMu orders the start of interactive shells, which need no manualMu.
	shellMu sync.Mutex
	// ptys are the person's commands running on a terminal.
	ptys map[string]*ptyRun
	// unclaimed is a finished session opened from its record to view: its
	// first write claims it. held is a claim taken for workbench work alone,
	// released by release after a quiet spell with the end it was opened with.
	unclaimed atomic.Bool
	// ownerGone is set by a revoke or a failed owner check; it holds wakes and
	// suggestions until the owner's own turn or a later owner check clears it.
	ownerGone atomic.Bool
	// fenced is set once another process has taken the session over: nothing
	// more is written for it here.
	fenced   atomic.Bool
	claimMu  sync.Mutex
	holdMu   sync.Mutex // guards held and release; a write takes it under claimMu
	held     bool
	release  *time.Timer
	priorEnd json.RawMessage
	// provider and model are what the session runs on now; a switch changes them under mu.
	provider string
	model    string
	// fallback is a resumed session's move to the default because its recorded
	// provider no longer resolves, written by its next turn; guarded by claimMu.
	fallback *agent.ModelSwitched
	// beatStop ends the heartbeat that keeps this node's claim on the session
	// fresh while a run or a background child is live; guarded by mu.
	beatStop func()
	mu       sync.Mutex
}

// sessionRedactor is the redactor a session records with: the store read as it
// changes where Redact can do so, withholding every payload if it cannot be loaded.
func (s *Server) sessionRedactor() agent.Redactor {
	// Preferred: one that follows the store for the whole session, as bash does.
	if fresh, ok := s.opts.Redact.(interface {
		Session() (*secrets.Fresh, error)
	}); ok {
		red, err := fresh.Session()
		if err != nil {
			s.log.Error("the session's event payloads will be withheld", "err", err)
			return secrets.Withholding()
		}
		return red
	}
	live, ok := s.opts.Redact.(interface {
		Load() (*secrets.Redactor, error)
	})
	if !ok {
		return s.opts.Redact
	}
	red, err := live.Load()
	if err != nil {
		s.log.Error("the session's event payloads will be withheld", "err", err)
		return secrets.Withholding()
	}
	return red
}

func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if v := reflect.ValueOf(opts.Redact); !v.IsValid() || v.Kind() == reflect.Pointer && v.IsNil() {
		opts.Redact = secrets.Default().Live()
	}
	st := opts.Store
	// '#' separates the node id from an incarnation's token in a holder.
	if strings.Contains(opts.NodeID, "#") {
		opts.Logger.Warn("the node id holds '#', which separates a lease's incarnation; it is used with '_' in its place", "node_id", opts.NodeID)
		opts.NodeID = strings.ReplaceAll(opts.NodeID, "#", "_")
	}
	holder := holderID(opts.NodeID)
	// On Postgres every append is fenced on this process's lease, in the
	// insert itself: a process that lost a session writes nothing into it,
	// and a refusal fences the session here.
	var srv *Server
	if pg, ok := st.(*store.Postgres); ok {
		st = pg.HeldBy(holder, func(id string) {
			if srv != nil {
				srv.leaseRefused(id)
			}
		})
	} else if _, durable := st.(SessionResumer); durable {
		// Another durable store is used as it is: sessions are held and
		// heartbeated, but its appends are not fenced on the lease.
		opts.Logger.Warn("the event store does not fence appends on a session's lease; " +
			"a process that lost a session could still write to its record until its next heartbeat")
	}
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
		opts:     opts,
		store:    tapped,
		log:      opts.Logger,
		running:  make(map[string]*liveSession),
		starting: map[string]bool{},
		holder:   holder,
		// Ten sign-in attempts a minute is far beyond what a person typing a
		// password needs, and far below what makes guessing viable.
		signinLimiter:  newLimiter(10, time.Minute),
		sessionLimiter: newLimiter(60, time.Minute),
		state: &mutable{
			registry: opts.Registry,
			skills:   opts.SkillRegistry,
			agents:   opts.Agents,
			gateway:  opts.Gateway,
			cfg:      opts.Config,
		},
	}
	if rec, ok := st.(SessionRecorder); ok {
		s.sessions = rec
	}
	srv = s
	if local := s.LocalAuth(); local != nil {
		local.OnChange(func(username string) {
			s.releaseStale(local, username)
			s.RecheckStreams()
		})
	}
	return s
}

// under is the store under any event tap, where the optional interfaces are.
func (s *Server) under() EventStore {
	if t, ok := s.store.(tapStore); ok {
		return t.EventStore
	}
	return s.store
}

// leaseRefused fences the session a store refused an append for: the store
// found it held by another process, or let go.
func (s *Server) leaseRefused(id string) {
	s.mu.RLock()
	live := s.running[id]
	s.mu.RUnlock()
	if live != nil {
		go s.fence(live) // not under the recorder that is appending now
	}
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
	mux.HandleFunc("POST /v1/sessions/{id}/folder", s.createFolder)
	mux.HandleFunc("POST /v1/sessions/{id}/rename", s.renamePath)
	mux.HandleFunc("POST /v1/sessions/{id}/delete", s.deletePath)
	mux.HandleFunc("GET /v1/sessions/{id}/search", s.searchSession)
	mux.HandleFunc("GET /v1/sessions/{id}/original", s.originalFile)
	mux.HandleFunc("POST /v1/sessions/{id}/accept", s.acceptChange)
	mux.HandleFunc("POST /v1/sessions/{id}/pty", s.startPTY)
	mux.HandleFunc("GET /v1/sessions/{id}/pty/{pty}", s.streamPTY)
	mux.HandleFunc("POST /v1/sessions/{id}/pty/{pty}/input", s.writePTY)
	mux.HandleFunc("POST /v1/sessions/{id}/pty/{pty}/resize", s.resizePTY)
	mux.HandleFunc("DELETE /v1/sessions/{id}/pty/{pty}", s.killPTY)
	mux.HandleFunc("POST /v1/sessions/{id}/messages", s.postMessage)
	mux.HandleFunc("GET /v1/sessions/{id}/tasks", s.listTasks)
	mux.HandleFunc("POST /v1/sessions/{id}/tasks/{task}/cancel", s.cancelTask)
	mux.HandleFunc("POST /v1/sessions/{id}/wake", s.setWake)
	mux.HandleFunc("GET /v1/sessions/{id}/queue", s.listQueue)
	mux.HandleFunc("GET /v1/sessions/{id}/state", s.sessionState)
	mux.HandleFunc("DELETE /v1/sessions/{id}/queue/{qid}", s.cancelQueued)
	mux.HandleFunc("POST /v1/sessions/{id}/upload", s.uploadFile)
	// Uploading before a session exists: see uploadFile for why a placeholder
	// session was the wrong answer.
	mux.HandleFunc("POST /v1/uploads", s.uploadFile)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.deleteSession)
	mux.HandleFunc("POST /v1/sessions/{id}/title", s.renameSession)
	mux.HandleFunc("POST /v1/sessions/{id}/fork", s.forkSession)
	mux.HandleFunc("GET /v1/sessions/{id}/export", s.exportSession)

	// Administrative routes, gated per-route on group membership rather than
	// by wrapping the whole mux — see rbac.go for why that distinction
	// matters. mux.Handle rather than HandleFunc because each is wrapped.
	mux.Handle("GET /v1/admin/users", s.Admin(s.listUsers))
	mux.Handle("POST /v1/admin/users/admin", s.Admin(s.setUserAdmin))
	mux.Handle("GET /v1/admin/settings", s.Admin(s.getSettings))
	mux.Handle("POST /v1/admin/skills/reload", s.Admin(s.reloadSkills))
	mux.Handle("POST /v1/admin/agents/reload", s.Admin(s.reloadAgents))
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
		// Sign-out is a POST, behind the same-origin check, so another page
		// cannot end a session with a link or an image; GET asks first.
		mux.HandleFunc("GET /logout", s.serveSignOut)
		mux.HandleFunc("POST /logout", s.signOut)
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
	case s.authMiddleware().ProxyMode():
		// The proxy owns sign-in and sign-out; whoami reports whom it named.
		mux.HandleFunc("GET /v1/whoami", s.whoami)
		mux.HandleFunc("GET /login", s.authDisabledPage)
		if u := s.opts.Config.Auth.ProxyLogoutURL; u != "" {
			mux.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, u, http.StatusFound)
			})
		} else {
			mux.HandleFunc("GET /logout", s.authDisabledPage)
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
	mux.HandleFunc("GET /favicon.svg", serveFaviconSVG)
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
	handler := s.mustChangeGate(mux)
	handler = s.requireGroup(handler)
	handler = s.withMiddleware(handler) // reads identity, logs

	// Sign-in is throttled OUTSIDE authentication, because an unauthenticated
	// attacker is precisely who this limits: by the time the auth layer has
	// rejected a password, the bcrypt comparison has already been paid for.
	authed := s.authMiddleware().Wrap(handler) // establishes identity
	limited := s.throttle(s.accountOutage(authed))

	// Origin is checked before anything reads a cookie, and headers are set
	// outermost so they are present on rejections too — an error response is
	// still a response a browser will act on.
	guarded := sameOrigin(s.opts.Config.Server.AllowedOrigins, s.log)(limited)
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

// accountOutage answers 503 when a local session's account cannot be read, so
// an outage of the account store is not mistaken for a sign-in that ended.
func (s *Server) accountOutage(next http.Handler) http.Handler {
	local := s.LocalAuth()
	if local == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health says whether the server is up, and sign-out must work regardless.
		if r.URL.Path != "/v1/health" && r.URL.Path != "/logout" {
			if _, routed := auth.FromContext(r.Context()); !routed && errors.Is(local.Verify(r), auth.ErrAccountUnchecked) {
				WriteError(w, http.StatusServiceUnavailable, auth.ErrAccountUnchecked.Error())
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authMiddleware builds the identity layer from config unless one was injected.
func (s *Server) authMiddleware() auth.Middleware {
	var mw auth.Middleware
	if s.opts.Auth != nil {
		mw = *s.opts.Auth
	} else {
		// Sign-in itself must be reachable without being signed in, or the
		// only way in is barred by the thing it unlocks.
		mw = auth.Middleware{PublicPaths: append(PublicPaths(), "/auth/callback", "/v1/signin", "/v1/signup")}
		if s.opts.Config.Auth.Mode == "proxy" {
			mw.TrustHeaders = true
		}
	}
	if group := s.opts.Config.Auth.RequireGroup; group != "" {
		// Checked once someone is identified, so a non-member's session is
		// ended and the person told why, rather than every request refused.
		next := mw.Check
		mw.Check = func(ctx context.Context, id *auth.Identity) error {
			if !slices.Contains(id.Groups, group) {
				return notMember(group)
			}
			if next != nil {
				return next(ctx, id)
			}
			return nil
		}
	}
	return mw
}

// notMember is what a signed-in person outside auth.require_group is told.
func notMember(group string) error {
	return fmt.Errorf("your account is not in the %s group, which this server requires; ask an administrator to add you", group)
}

// requireGroup refuses an identified caller outside auth.require_group, leaving
// the paths needed to sign in or out open, since nobody has a group before that.
func (s *Server) requireGroup(next http.Handler) http.Handler {
	group := s.opts.Config.Auth.RequireGroup
	if group == "" {
		return next
	}
	public := append(slices.Clone(s.authMiddleware().PublicPaths), "/logout")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(public, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if id, ok := auth.FromContext(r.Context()); ok && slices.Contains(id.Groups, group) {
			next.ServeHTTP(w, r)
			return
		}
		why := notMember(group).Error()
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/?refused="+url.QueryEscape(why), http.StatusFound)
			return
		}
		WriteJSON(w, http.StatusForbidden, map[string]string{
			"error": why, "reason": "requires group " + group})
	})
}

// withMiddleware applies identity and logging. Authentication is delegated to
// the enterprise IdP in production (docs/ops/air-gap.md §4); this reads the
// identity headers a reverse proxy sets after authenticating.
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Identity comes from the auth layer, which has already verified it.
		id, _ := auth.FromContext(r.Context())
		user, tenant := s.callerOf(r.Context(), id)
		ctx := context.WithValue(r.Context(), ctxUser, user)
		ctx = context.WithValue(ctx, ctxTenant, tenant)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// A caller who names no one could create sessions it can never open again.
		if auth.OwnsNothing(user) && strings.HasPrefix(r.URL.Path, "/v1/") {
			WriteError(rec, http.StatusUnauthorized, "the request names no user")
			s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
				"user", user, "tenant", tenant, "duration", time.Since(start))
			return
		}

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

// callerOf is the user and tenant a request acts as, given its identity.
func (s *Server) callerOf(ctx context.Context, id *auth.Identity) (user, tenant string) {
	return id.Owner(), s.tenantFor(ctx, id)
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
	// Workbench opens a session with no prompt, idle until a message arrives,
	// so the workbench has a sandbox to work in before anyone asks the agent.
	Workbench bool `json:"workbench,omitempty"`
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
		badBody(w, err, "invalid JSON body")
		return
	}
	idle := req.Workbench && strings.TrimSpace(req.Prompt) == ""
	if strings.TrimSpace(req.Prompt) == "" && !idle {
		WriteError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if !validClientID(req.ClientID) {
		WriteError(w, http.StatusBadRequest, "client_id is up to 64 letters, digits, - and _")
		return
	}
	spec := StartSpec{
		Prompt: req.Prompt, Mode: req.Mode, Provider: req.Provider, ClientID: req.ClientID,
		User: UserOf(r.Context()), Tenant: TenantOf(r.Context()),
	}
	start := s.StartSession
	if idle {
		start = s.openWorkbench
	}
	sessionID, err := start(r.Context(), spec)
	if err != nil {
		switch {
		case errors.Is(err, errDraining):
			w.Header().Set("Retry-After", "5")
			WriteError(w, http.StatusServiceUnavailable, "server is shutting down; retry")
		case errors.Is(err, errHoldFailed):
			writeHoldFailed(w)
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

	// Refused before the row is written, so a refused start leaves nothing.
	if s.draining.Load() {
		return "", errDraining
	}
	sessionID := newSessionID()
	defer s.startingDone(s.startingNow(sessionID))
	if err := s.persistSession(ctx, sessionID, spec, mode, adapter, true); err != nil {
		return "", err
	}

	rec := agent.NewRecorder(s.store, sessionID, "")
	rec.Redact = s.sessionRedactor()
	live, loop, err := s.buildLive(sessionID, spec, mode, adapter, registry, skillReg, rec)
	if err != nil {
		return "", err
	}
	// Names the provider, so a resume cannot mistake it for another serving the same model.
	if _, err := rec.Record(agent.EvSessionStarted, agent.ActorSystem, agent.Trusted, map[string]string{
		"origin": "chat", "workspace": s.opts.Workspace, "model": adapter.Profile().Name, "mode": mode,
		"provider": live.provider,
	}); err != nil {
		s.forgetUnstarted(sessionID)
		return "", fmt.Errorf("record session start: %w", err)
	}

	runCtx, cancelCause := context.WithCancelCause(context.Background())
	cancel := func() { cancelCause(nil) }
	live.Cancel = cancel
	live.cancel = cancel
	live.cancelCause = cancelCause
	live.Turns = 1
	live.ran = make(chan struct{})

	s.mu.Lock()
	// Re-checked under the lock the shutdown takes to cancel running turns,
	// so a session is either cancelled by it or never started.
	if s.draining.Load() {
		s.mu.Unlock()
		s.forgetUnstarted(sessionID)
		return "", errDraining
	}
	s.running[sessionID] = live
	s.mu.Unlock()
	// Held, with its liveness, before it runs: a process that cannot say it
	// holds the session must not run it, or another could take it as orphaned.
	if err := s.holdNode(live); err != nil {
		s.mu.Lock()
		delete(s.running, sessionID)
		s.mu.Unlock()
		cancel()
		s.forgetUnstarted(sessionID)
		return "", fmt.Errorf("%w: %w", errHoldFailed, err)
	}

	go func() {
		defer cancel()
		live.undo.BeginTurn()
		reason, err := loop.RunMessage(runCtx, agent.Message{Text: spec.Prompt, ClientID: spec.ClientID})
		for live.settle(runCtx, reason, err) {
			reason, err = loop.RunQueued(runCtx)
		}
		waitSuggestion(live)
		s.releaseAndLetGo(live)
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

// persistSession writes the session row. Events reference sessions, so the
// row must exist before the first one.
func (s *Server) persistSession(ctx context.Context, id string, spec StartSpec, mode string, adapter model.Adapter, hold bool) error {
	if s.sessions == nil {
		return nil
	}
	// A session that runs at once is held from its row's first moment: no
	// other process may take it for an orphan before its hold is written.
	holder := ""
	if _, ok := s.liveness(); ok && hold {
		holder = s.holder
	}
	if err := s.sessions.CreateSession(ctx, store.SessionRecord{
		ID: id,
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
		Holder:    holder,
	}); err != nil {
		return fmt.Errorf("persist session: %w", err)
	}
	return nil
}

// openWorkbench creates a session with no prompt: the same workspace, policy,
// sandbox, owner and record as any other, idle until a message arrives.
func (s *Server) openWorkbench(ctx context.Context, spec StartSpec) (string, error) {
	mode, ok := requestMode(s.opts.Config.Permissions.Mode, spec.Mode)
	if !ok {
		return "", errBadMode
	}
	if s.draining.Load() {
		return "", errDraining
	}
	registry, skillReg, _ := s.state.snapshot()
	adapter := s.opts.Adapter
	if spec.Provider != "" {
		a, err := s.resolveProvider(spec.Provider)
		if err != nil {
			return "", fmt.Errorf("provider: %w", err)
		}
		adapter = a
	}
	sessionID := newSessionID()
	defer s.startingDone(s.startingNow(sessionID))
	rec := agent.NewRecorder(s.store, sessionID, "")
	rec.Redact = s.sessionRedactor()
	// Built before the row is written, so a failure leaves no empty session listed.
	live, _, err := s.buildLive(sessionID, spec, mode, adapter, registry, skillReg, rec)
	if err != nil {
		return "", err
	}
	if err := s.persistSession(ctx, sessionID, spec, mode, adapter, false); err != nil {
		return "", err
	}
	live.State = "idle"
	// The first event, so the session has a record to be resumed from.
	if _, err := rec.Record(agent.EvSessionStarted, agent.ActorSystem, agent.Trusted, map[string]string{
		"origin": "workbench", "workspace": s.opts.Workspace, "model": adapter.Profile().Name, "mode": mode,
		"provider": live.provider,
	}); err != nil {
		// An empty session left listed would be one nobody can open.
		if del, ok := s.under().(agent.SessionDeleter); ok {
			_ = del.DeleteSession(sessionID)
		}
		return "", fmt.Errorf("record session start: %w", err)
	}
	s.mu.Lock()
	if s.draining.Load() {
		s.mu.Unlock()
		s.forgetUnstarted(sessionID)
		return "", errDraining
	}
	s.running[sessionID] = live
	s.mu.Unlock()
	return sessionID, nil
}

// startingNow marks a session as being started, before its row is written,
// and returns its id for startingDone.
func (s *Server) startingNow(id string) string {
	s.mu.Lock()
	s.starting[id] = true
	s.mu.Unlock()
	return id
}

// startingDone ends the mark once the session is in running, or refused.
func (s *Server) startingDone(id string) {
	s.mu.Lock()
	delete(s.starting, id)
	s.mu.Unlock()
}

// forgetUnstarted removes a session refused after its row was written, so no
// session is left listed that never ran and never ends.
func (s *Server) forgetUnstarted(sessionID string) {
	if del, ok := s.under().(agent.SessionDeleter); ok {
		_ = del.DeleteSession(sessionID)
	}
}

// newPolicy builds the engine from the operator's rules. One constructor, so
// a rule cannot bind the agent and not the endpoints that serve files.
func (s *Server) newPolicy(mode policy.Mode) *policy.Engine {
	pol := policy.New(mode)
	pol.Managed = s.opts.Config.Managed
	// Path rules relative to the workspace; buildLive gives a session its own roots.
	workspace := s.opts.Workspace
	pol.Roots = func() []string { return []string{workspace} }
	_ = pol.AddDeny(s.opts.Config.Permissions.Deny...)
	_ = pol.AddAsk(s.opts.Config.Permissions.Ask...)
	_ = pol.AddAllow(s.opts.Config.Permissions.Allow...)
	_ = pol.AllowGitExtensions(s.opts.Config.Permissions.GitExtensions...)
	pol.AskReadOnly = webfetch.AskReadOnly(s.opts.Config.WebFetch.Enabled, s.opts.Config.WebFetch.AllowedHosts)
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
	pol.Roots = sess.PolicyRoots
	undo := agent.NewUndoLog(sess.RestoreFile, sess.RemoveFile)
	// The server reads only each file's baseline, so one copy per file is kept, not one per edit.
	undo.KeepFirst = true
	sess.Checkpoint = undo.Record

	live := &liveSession{
		ID: sessionID, User: spec.User, Tenant: spec.Tenant,
		Created: time.Now(), Prompt: spec.Prompt, State: "running",
		// Replaced when a turn starts; a session opened idle has nothing to cancel.
		Cancel:  func() {},
		allowed: map[string]bool{},
		durable: s.approvalStore(),
		undo:    undo,
		// A spec with no provider runs on the configured default.
		provider: orDefaultStr(spec.Provider, s.opts.Config.Model.Default),
		model:    adapter.Profile().Name,
	}
	// Once another process has taken the session over, nothing more is
	// written to its record from here.
	rec.Gate = func() error {
		if live.fenced.Load() {
			return errLeaseLost
		}
		// A session let go after its run is claimed again by its next write,
		// and every later write keeps a workbench hold from running out.
		return s.claimForWrite(sessionID, live)
	}

	// The prompt, loop settings and budget as the CLI builds them. The prompt
	// is set once the session's own tools are bound, so it names only those.
	cfg := toolset.LoopConfig(s.opts.Config, "")
	toolset.Police(s.opts.Extensions, pol, sessionID)

	var approver agent.Approver = live
	if spec.Unattended {
		approver = agent.AutoApprove{Yes: false}
	}
	budget := toolset.Budget(s.opts.Config)
	own := s.sessionTools(sessionID, spec, mode, adapter, registry, skillReg, pol, sess, budget, cfg, rec)
	cfg.SystemPrompt = toolset.SystemPrompt(s.opts.Workspace, adapter, s.skillListing(skillReg), own.Names())
	loop := agent.NewLoop(adapter, own, pol, approver, sess, rec, cfg)
	// Background children belong to the session. An unattended run has
	// nobody to come back to it, so its children are joined: it waits for
	// them, and its end, and OnEnd, come after theirs.
	ceiling := agent.WakeAuto
	if spec.Unattended {
		ceiling = agent.WakeOff
	}
	agent.NewBackground(loop, toolset.BackgroundPolicy(s.opts.Config, ceiling))
	loop.Background.SetHooks(agent.BackgroundHooks{
		Idle:    func(ev agent.IdleEvent) { s.onIdle(live, ev) },
		CanWake: func() (bool, string) { return s.canWake(live) },
		// The owner lookup may take seconds: made before the run lock, in
		// every wake mode, and read by CanWake.
		BeforeIdle: func() { s.checkOwner(live) },
		Wake:       func(ids []string) bool { return s.wake(live, ids) },
	})
	// Asked again inside a woken run, before each model call and approval:
	// access withdrawn while it runs ends it.
	loop.OwnerActive = func() bool {
		active := s.ownerActive(live)
		if !active {
			live.ownerGone.Store(true)
		}
		return active
	}
	loop.Compactor = agent.NewCompactor(adapter, cfg.CompactAt)
	toolset.Summarize(loop.Compactor, s.opts.Extensions, sessionID)
	loop.Budget = budget
	loop.Provider = live.provider
	// A person at the console or the IDE is offered a next prompt; an unattended run is not.
	if !spec.Unattended {
		loop.Suggest = toolset.Suggester(s.opts.Config)
		if loop.Suggest != nil {
			// A revoked owner gets no suggestion, even from a run still ending.
			loop.Suggest.Hold = live.ownerGone.Load
		}
	}
	live.Loop = loop
	return live, loop, nil
}

// sessionTools is the shared registry with this session's own tools bound to
// it: the skill tool over the skills loaded now, and task and tasks spawning
// into this session's workspace, record, policy and budget.
//
// A subagent has no approver of its own: its asks go to the approver of the
// loop that spawned it, which for an attended session is the person in the
// console, through the same pending request and answer as the parent's, and
// for an unattended one the refuser. None is approved on its behalf.
func (s *Server) sessionTools(sessionID string, spec StartSpec, mode string, adapter model.Adapter,
	registry *tools.Registry, skillReg *skills.Registry, pol *policy.Engine, sess *tools.Session,
	budget *agent.Budget, cfg agent.Config, rec *agent.Recorder) *tools.Registry {
	if registry == nil {
		registry = tools.NewRegistry()
	}
	// Bound per session, so a skill reloaded in settings reaches the next session's tool too.
	if skillReg != nil {
		registry = registry.Clone()
		registry.Remove("skill")
		if skillReg.Len() > 0 {
			registry.Add(toolset.SkillTool(skillReg))
		}
	}
	// The agent types are those loaded when the session starts; a reload
	// reaches the next session, never this one mid-conversation.
	f := &agent.SubagentFactory{
		Adapter: adapter, Policy: pol, Session: sess, Budget: budget, Config: cfg,
		Workspace: sess.Root, Redact: rec.Redact, Background: true,
		Store:       s.subagentStore(sessionID, spec, mode),
		Definitions: s.state.agentDefs(),
		// Only a provider this server offers sessions, by name, as the
		// console's model picker; a subagent never falls back to another.
		Models: s.subagentModel, ModelNames: s.providerNames(),
	}
	return toolset.Subagents(registry, f, s.opts.Config.Limits.MaxParallelSubagents)
}

// subagentStore is where a session's subagents record. With session rows it
// writes each child's row as the session owner's, in the session's tenant and
// naming the session that spawned it, so a child is served only to whoever may
// see its parent, is left out of lists, and is deleted with it.
func (s *Server) subagentStore(sessionID string, spec StartSpec, mode string) agent.Store {
	if s.sessions == nil {
		return s.store
	}
	tenant, user, workspace := storeTenant(s.opts.Config, spec.Tenant), spec.User, s.opts.Workspace
	return subSessions{EventStore: s.store, owns: func(ctx context.Context, childID, parentID string) (bool, error) {
		// A resume is only for the session that started the child, as its owner.
		getter, ok := s.sessions.(interface {
			GetSession(ctx context.Context, id string) (store.SessionRecord, error)
		})
		if !ok {
			return true, nil // no rows to say more than the record's parent link
		}
		rec, err := getter.GetSession(ctx, childID)
		if err != nil {
			return false, nil //nolint:nilerr // not found and not readable answer alike: no such task
		}
		return rec.ParentID == parentID && ownsSession(rec.Tenant, rec.User, tenant, user), nil
	}, create: func(ctx context.Context, id, parentID, description string) error {
		if parentID == "" {
			parentID = sessionID
		}
		return s.sessions.CreateSession(ctx, store.SessionRecord{
			ID: id, Tenant: tenant, User: user, Workspace: workspace,
			Model: "subagent", Mode: mode, ParentID: parentID,
			Prompt: description, StartedAt: time.Now().UTC(),
		})
	}}
}

// subSessions gives the subagent factory the parent's owner for each child row.
type subSessions struct {
	EventStore
	create func(ctx context.Context, id, parentID, description string) error
	owns   func(ctx context.Context, childID, parentID string) (bool, error)
}

var _ agent.SubSessionChecker = subSessions{}

func (c subSessions) SubSessionOf(ctx context.Context, childID, parentID string) (bool, error) {
	return c.owns(ctx, childID, parentID)
}

var _ agent.SessionCreator = subSessions{}

func (c subSessions) CreateSubagentSession(ctx context.Context, id, parentID, description string) error {
	return c.create(ctx, id, parentID, description)
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
//
// With claim false the session is opened without claiming it, so its stored
// end stands until claimOpened runs for a message.
func (s *Server) resumeSession(ctx context.Context, id string, prompt, user, tenant string, claim bool) (*liveSession, error) {
	if s.draining.Load() {
		return nil, errDraining
	}
	events, err := s.store.Events(id)
	if err != nil {
		return nil, fmt.Errorf("read record: %w", err)
	}
	// A subagent's record goes on only through the session that started it.
	if _, child := agent.SubagentRecord(events); len(events) == 0 || child {
		return nil, errNoSession
	}
	// Ownership and mode come from the stored row when there is one.
	var rec store.SessionRecord
	if g, ok := s.sessions.(sessionGetter); ok {
		// By id, so a session older than any bounded list still opens.
		r, err := g.GetSession(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil, errNoSession
		}
		if err != nil {
			return nil, fmt.Errorf("read sessions: %w", err)
		}
		rec = r
	} else if s.sessions != nil {
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
	claimer, durable := s.sessions.(SessionResumer)
	// A session a crashed process left open is taken over and reconciled:
	// its lost children and its run are given the ends they never recorded,
	// which releases the row to be claimed as any finished session is.
	if durable && claim && rec.EndedAt == nil && s.recoverOrphan(ctx, id, events) {
		if events, err = s.store.Events(id); err != nil {
			return nil, fmt.Errorf("read record: %w", err)
		}
		rec.EndedAt = &events[len(events)-1].CreatedAt
	}
	if durable && !claim && rec.EndedAt == nil {
		return nil, errBusySession // running elsewhere: not opened here, even to view
	}
	if durable && claim {
		claimed, err := claimer.ClaimResume(ctx, id)
		if err != nil {
			return nil, err
		}
		if !claimed {
			return nil, errBusySession
		}
	}

	msgs, err := agent.Fork(events, 0)
	if err != nil && messaged(events) {
		return nil, fmt.Errorf("rebuild conversation: %w", err)
	}
	mode, ok := requestMode(s.opts.Config.Permissions.Mode, rec.Mode)
	if !ok {
		mode, _ = requestMode(s.opts.Config.Permissions.Mode, "")
	}
	registry, skillReg, _ := s.state.snapshot()
	recorder := agent.NewRecorder(s.store, id, "")
	recorder.Redact = s.sessionRedactor()
	recorder.Advance(events[len(events)-1].Seq)

	spec := StartSpec{Prompt: rec.Prompt, Mode: mode, User: user, Tenant: tenant}
	if spec.Prompt == "" {
		spec.Prompt = prompt
	}
	adapter, provider, lost := s.recordedProvider(id, events, rec.Model)
	spec.Provider = provider
	live, loop, err := s.buildLive(id, spec, mode, adapter, registry, skillReg, recorder)
	if err != nil {
		return nil, err
	}
	if lost {
		// Recorded by the next turn, once claimed: opening a session to view it writes nothing.
		live.fallback = &agent.ModelSwitched{Provider: s.opts.Config.Model.Default,
			Model: adapter.Profile().Name, From: orDefaultStr(agent.LastModel(events), rec.Model)}
	}
	loop.SetHistory(msgs, rec.Turns)
	if end, ok := agent.LastEnd(events); ok {
		loop.CarryUsage(end)
	}
	// Results a background child left that the conversation never took,
	// such as one that finished before a drain, arrive at the next boundary,
	// once the session is claimed: a session opened only to view it writes
	// nothing, and an idle delivery would. The allowance goes on from what
	// the session already spent.
	if !durable || claim {
		loop.QueueNotices(agent.PendingNotices(events, s.store.Events))
	}
	loop.Budget.Carry(agent.CarriedSpend(events))
	live.Turns = rec.Turns
	live.title = agent.TitleOf(events)
	// Idle until the caller's prompt starts it: postMessage treats a running
	// session as one to steer, and there is nothing running yet to steer.
	live.State = "done"
	live.Reason = agent.TerminalReason(rec.TerminalReason)
	live.priorEnd = priorEnd(events, rec)
	if durable {
		// Claimed or not: a claim given back unused leaves the session to be claimed again.
		live.unclaimed.Store(!claim)
		recorder.Gate = func() error {
			if live.fenced.Load() {
				return errLeaseLost
			}
			return s.claimForWrite(id, live)
		}
	}

	s.mu.Lock()
	// Re-checked under the lock: a drain that began while the record was
	// being read must not leave a turn running on a node that is exiting.
	if s.draining.Load() {
		s.mu.Unlock()
		if durable && claim {
			s.endAsBefore(id, live) // the claim is given back, ended as it was
		}
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

// recordedProvider is the adapter a continued session runs on: the provider
// its record last named, so a switch survives a restart or another node.
// Anything that no longer resolves falls back to the default, and says so:
// lost reports it, for the record.
func (s *Server) recordedProvider(id string, events []agent.Event, rowModel string) (a model.Adapter, provider string, lost bool) {
	name := agent.ProviderOf(events)
	if name == "" && rowModel != "" {
		// A record from before session.started named its provider: only the row's model is known.
		var found []string
		for n, p := range s.opts.Config.Model.Providers {
			// Relies on every adapter naming its profile after the configured model.
			if p.Model == rowModel {
				found = append(found, n)
			}
		}
		if rowModel == s.opts.Adapter.Profile().Name && len(found) <= 1 {
			return s.opts.Adapter, "", false // the default's model, which no other provider serves
		}
		if len(found) != 1 {
			// None serves it any more, or several do and the row cannot say which:
			// the default answers, and the record says so.
			s.log.Warn("the session's model does not name one configured provider; continuing on the default",
				"session", id, "model", rowModel, "providers", len(found))
			return s.opts.Adapter, "", true
		}
		name = found[0]
	}
	if name == "" || name == s.opts.Config.Model.Default {
		return s.opts.Adapter, "", false
	}
	a, err := s.resolveProvider(name)
	if err != nil {
		s.log.Warn("the session's model is not available here; continuing on the default",
			"session", id, "provider", name, "error", err)
		return s.opts.Adapter, "", true
	}
	return a, name, false
}

// manualHold is how long a claim taken for workbench work alone is kept
// after its last write, before the session is released as it was found.
var manualHold = 2 * time.Minute

// priorEnd is the end a session was opened with, recorded again on release.
func priorEnd(events []agent.Event, rec store.SessionRecord) json.RawMessage {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == agent.EvSessionEnded {
			return withoutSuggesting(events[i].Payload)
		}
	}
	reason := agent.TerminalReason(rec.TerminalReason)
	if reason == "" {
		reason = agent.TermCompleted
	}
	end, _ := json.Marshal(agent.SessionEnded{Reason: reason, Turns: rec.Turns})
	return end
}

// withoutSuggesting is an end recorded again: no suggestion follows it.
func withoutSuggesting(p json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(p, &m) != nil || m["suggesting"] == nil {
		return p
	}
	delete(m, "suggesting")
	if out, err := json.Marshal(m); err == nil {
		return out
	}
	return p
}

// claimForWrite is an unclaimed session's recorder gate: the first write
// claims the session and holds it for workbench work until it goes quiet.
func (s *Server) claimForWrite(id string, live *liveSession) error {
	if !live.unclaimed.Load() {
		live.holdMu.Lock()
		if live.held {
			live.release.Reset(manualHold)
		}
		live.holdMu.Unlock()
		return nil
	}
	if s.draining.Load() {
		return errDraining
	}
	newly, err := s.claimOpened(context.Background(), id, live)
	if err != nil || !newly {
		return err
	}
	s.holdClaim(id, live)
	return nil
}

// holdClaim keeps a claim taken for workbench work until it goes quiet, then
// releases the session with the end it was opened with.
func (s *Server) holdClaim(id string, live *liveSession) {
	live.mu.Lock()
	turn := live.State == "running" || live.State == "waiting_approval"
	live.mu.Unlock()
	if turn {
		return // the turn records its own end; a hold would end it again
	}
	live.holdMu.Lock()
	defer live.holdMu.Unlock()
	live.held = true
	if live.release == nil {
		live.release = time.AfterFunc(manualHold, func() { s.releaseHeld(id, live, false) })
	} else {
		live.release.Reset(manualHold)
	}
}

// claimOpened claims a session opened unclaimed, and takes up anything another
// process recorded since it was opened. It reports whether it claimed now.
func (s *Server) claimOpened(ctx context.Context, id string, live *liveSession) (bool, error) {
	live.claimMu.Lock()
	defer live.claimMu.Unlock()
	return s.claimLocked(ctx, id, live)
}

// claimLocked is claimOpened for a caller that holds claimMu.
func (s *Server) claimLocked(ctx context.Context, id string, live *liveSession) (bool, error) {
	if !live.unclaimed.Load() {
		return false, nil
	}
	claimed, err := s.sessions.(SessionResumer).ClaimResume(ctx, id)
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, errBusySession
	}
	// Every claim is held with this process's liveness, a workbench hold's
	// included, so no other process takes the session for a crashed one; a
	// claim that cannot record it is given back rather than kept unseen.
	if err := s.holdNode(live); err != nil {
		s.giveBack(id, live)
		return false, fmt.Errorf("%w: %w", errHoldFailed, err)
	}
	events, err := s.store.Events(id)
	if err == nil && len(events) > 0 && events[len(events)-1].Seq != live.Loop.Recorder.LastAppended() {
		err = s.catchUp(live, events)
	}
	if err != nil {
		s.giveBack(id, live)
		return false, err
	}
	live.unclaimed.Store(false)
	// Owed results are queued only now it is claimed, so their delivery
	// (which writes) never has to claim from under the run lock.
	live.Loop.QueueNotices(agent.PendingNotices(events, s.store.Events))
	return true, nil
}

// catchUp rebuilds an opened session from its record as another process left it.
func (s *Server) catchUp(live *liveSession, events []agent.Event) error {
	msgs, err := agent.Fork(events, 0)
	if err != nil && messaged(events) {
		return fmt.Errorf("rebuild conversation: %w", err)
	}
	end, _ := agent.LastEnd(events)
	live.Loop.Recorder.Advance(events[len(events)-1].Seq)
	live.Loop.SetHistory(msgs, end.Turns)
	live.Loop.CarryUsage(end)
	live.Loop.Budget.Carry(agent.CarriedSpend(events))
	live.priorEnd = priorEnd(events, store.SessionRecord{})
	live.mu.Lock()
	live.Turns = max(live.Turns, end.Turns)
	live.mu.Unlock()
	return nil
}

// giveBack releases a claim taken in claimOpened that could not be used. The
// caller holds claimMu and the session is still marked unclaimed.
func (s *Server) giveBack(id string, live *liveSession) {
	live.unclaimed.Store(false) // the end below is this claim's own write
	s.endAsBefore(id, live)
	live.unclaimed.Store(true)
	s.releaseNodeIfQuiet(live)
}

// endAsBefore records the session's prior end again, so the row is released
// with the reason and totals it was opened with.
func (s *Server) endAsBefore(id string, live *liveSession) {
	if _, err := live.Loop.Recorder.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, live.priorEnd); err != nil {
		s.log.Warn("could not release a claimed session", "session", id, "error", err)
	}
}

// releaseHeld gives back a claim held for workbench work once no terminal is
// open and no turn has started; now releases it whatever is open, for a drain.
func (s *Server) releaseHeld(id string, live *liveSession, now bool) {
	live.claimMu.Lock()
	defer live.claimMu.Unlock()
	live.holdMu.Lock()
	held := live.held
	live.holdMu.Unlock()
	if !held {
		return
	}
	live.mu.Lock()
	// A result still owed keeps the claim: given back, it would be delivered
	// (a write) on a session nobody holds.
	busy := live.State == "running" || live.State == "waiting_approval" || live.Loop.Background.Owed() > 0
	for _, run := range live.ptys {
		select {
		case <-run.done:
		default:
			busy = true
		}
	}
	live.mu.Unlock()
	live.holdMu.Lock()
	if busy && !now {
		live.release.Reset(manualHold)
		live.holdMu.Unlock()
		return
	}
	live.release.Stop()
	live.held = false
	live.holdMu.Unlock()
	s.endAsBefore(id, live)
	live.unclaimed.Store(true)
	s.releaseNodeIfQuiet(live)
}

// takeOver ends a workbench hold when a message claims the session for a
// turn, which records its own end.
func (live *liveSession) takeOver() {
	live.holdMu.Lock()
	defer live.holdMu.Unlock()
	if live.held {
		live.held = false
		live.release.Stop()
	}
}

// listedReason is how a finished session's last run ended, such as completed,
// shutdown or deadline, for the list; "" while it is live.
func listedReason(state string, reason agent.TerminalReason) string {
	if state == "done" {
		return string(reason)
	}
	return ""
}

// messaged reports whether anyone has sent the session a message. One opened
// at the workbench may have none, and continues with an empty conversation.
func messaged(events []agent.Event) bool {
	for _, e := range events {
		if e.Type == agent.EvUserMessage {
			return true
		}
	}
	return false
}

var (
	errNoSession   = errors.New("session not found")
	errBusySession = errors.New("session is already running")
	// errDraining means this node is shutting down. It is not a failure: the
	// caller should be sent to a node that is still accepting work.
	errDraining = errors.New("server is draining")
)

type sessionSummary struct {
	ID     string `json:"id"`
	User   string `json:"user"`
	Tenant string `json:"tenant"`
	Prompt string `json:"prompt"`
	// Title is the name its owner gave it, recorded as session.renamed.
	Title string `json:"title,omitempty"`
	State string `json:"state"`
	// Reason is how a done session's last run ended, when the record says.
	Reason  string    `json:"reason,omitempty"`
	Created time.Time `json:"created"`
	// Mode is the permission mode the session was started in.
	Mode string `json:"mode,omitempty"`
	// Model is what the session runs on now; Provider its configured name,
	// known while the session is live here.
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	// Background counts the session's background tasks still running, and
	// PendingAsk is an approval waiting on its owner, run or not.
	Background int         `json:"background,omitempty"`
	PendingAsk *pendingAsk `json:"pending_ask,omitempty"`
}

// pendingAsk is what the session list says of an approval that is waiting.
type pendingAsk struct {
	Tool     string    `json:"tool"`
	Subagent string    `json:"subagent,omitempty"`
	Since    time.Time `json:"since"`
}

// activity is a live session's background count and waiting approval, for
// the list. The caller holds live.mu.
func (l *liveSession) activity() (int, *pendingAsk) {
	var ask *pendingAsk
	if p := l.pending; p != nil {
		ask = &pendingAsk{Tool: p.Tool, Subagent: p.Subagent, Since: p.Since}
	}
	return l.Loop.Background.Live(), ask
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	tenant := TenantOf(r.Context())
	user := UserOf(r.Context())

	// A durable store also returns sessions from before this process started,
	// which is what makes audit useful after a restart.
	if s.sessions != nil {
		records, err := s.listOwned(r.Context(), user, 200)
		if err == nil {
			out := make([]sessionSummary, 0, len(records))
			for _, rec := range records {
				// The store hands back every session it holds. Row-level
				// security scopes that by TENANT in Postgres, and not at all
				// in the memory store — neither scopes it by user. So the
				// filter has to be here, or one person's list of prompts
				// (which is a list of what they were working on, and often
				// what they uploaded) is shown to everyone else in the tenant.
				if !ownsSession(rec.Tenant, rec.User, tenant, user) || rec.ParentID != "" {
					continue
				}
				// Listed only once its routes answer: a row this process is still starting is left out.
				s.mu.RLock()
				_, held := s.running[rec.ID]
				unready := s.starting[rec.ID] && !held
				s.mu.RUnlock()
				if unready {
					continue
				}
				state, reason, prompt, title := "done", rec.TerminalReason, rec.Prompt, rec.Title
				modelName, provider := rec.Model, ""
				if rec.EndedAt == nil {
					state, reason = "running", ""
				}
				var bg int
				var ask *pendingAsk
				s.mu.RLock()
				if live, found := s.running[rec.ID]; found {
					live.mu.Lock()
					state, reason = live.State, listedReason(live.State, live.Reason)
					if prompt == "" {
						prompt = live.Prompt
					}
					modelName, provider = live.model, live.provider
					if live.title != "" {
						title = live.title
					}
					bg, ask = live.activity()
					live.mu.Unlock()
				}
				s.mu.RUnlock()
				out = append(out, sessionSummary{
					ID: rec.ID, User: rec.User, Tenant: rec.Tenant,
					Prompt: prompt, Title: title, State: state, Reason: reason, Created: rec.StartedAt, Mode: rec.Mode,
					Model: modelName, Provider: provider, Background: bg, PendingAsk: ask,
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
		// user check is the same rule s.session(r.Context(), ) applies to a single session,
		// applied to the list — they must agree, or the list advertises
		// sessions that then 404.
		if !ownsSession(l.Tenant, l.User, tenant, user) {
			continue
		}
		l.mu.Lock()
		bg, ask := l.activity()
		out = append(out, sessionSummary{
			ID: l.ID, User: l.User, Tenant: l.Tenant,
			Prompt: l.Prompt, Title: l.title, State: l.State, Reason: listedReason(l.State, l.Reason),
			Created: l.Created, Mode: string(l.Loop.Policy.Mode),
			Model: l.model, Provider: l.provider, Background: bg, PendingAsk: ask,
		})
		l.mu.Unlock()
	}
	WriteJSON(w, http.StatusOK, out)
}

// sessionStateResponse is one session's state, for a page watching it.
type sessionStateResponse struct {
	ID     string `json:"id"`
	State  string `json:"state"` // running | waiting_approval | idle | done
	Reason string `json:"reason,omitempty"`
	// Turns lets a page following an idle session see a turn it missed.
	Turns int `json:"turns,omitempty"`
}

// sessionState answers one session's state to its owner, so a page open on a
// session with no run can tell when the next one starts without listing the
// tenant's sessions. Anyone else is told it does not exist.
func (s *Server) sessionState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if live, ok := s.session(r.Context(), id, TenantOf(r.Context()), UserOf(r.Context())); ok {
		live.mu.Lock()
		out := sessionStateResponse{ID: id, State: live.State, Reason: listedReason(live.State, live.Reason), Turns: live.Turns}
		live.mu.Unlock()
		WriteJSON(w, http.StatusOK, out)
		return
	}
	if g, ok := s.sessions.(sessionGetter); ok && s.mayAccess(r, id) {
		if rec, err := g.GetSession(r.Context(), id); err == nil {
			out := sessionStateResponse{ID: id, State: "done", Reason: rec.TerminalReason}
			if rec.EndedAt == nil {
				out.State, out.Reason = "running", ""
			}
			WriteJSON(w, http.StatusOK, out)
			return
		}
	}
	WriteError(w, http.StatusNotFound, "session not found")
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
	if _, ok := s.session(r.Context(), id, tenant, user); ok {
		return true
	}
	if s.sessions == nil {
		return false
	}
	// Found by id where the store can: a list is bounded and leaves subagents out.
	if g, ok := s.sessions.(sessionGetter); ok {
		rec, err := g.GetSession(r.Context(), id)
		return err == nil && ownsSession(rec.Tenant, rec.User, tenant, user)
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

// scheduledForAdmin reports whether the caller is an administrator and id is
// a scheduled run in their tenant: a session no identity owns, which only an
// administrator may read, never continue.
func (s *Server) scheduledForAdmin(r *http.Request, id string) (store.SessionRecord, bool) {
	g, ok := s.sessions.(sessionGetter)
	if !ok || !s.IsAdmin(r) {
		return store.SessionRecord{}, false
	}
	rec, err := g.GetSession(r.Context(), id)
	if err != nil || rec.Tenant != TenantOf(r.Context()) || !strings.HasPrefix(rec.User, auth.SchedulePrefix) {
		return store.SessionRecord{}, false
	}
	return rec, true
}

// mayRead is mayAccess for reading a session's record, which an administrator
// may also do for a scheduled run.
func (s *Server) mayRead(r *http.Request, id string) bool {
	if s.mayAccess(r, id) {
		return true
	}
	_, ok := s.scheduledForAdmin(r, id)
	return ok
}

// readable is mayRead for a handler that serves the record, auditing an
// administrator's read of a scheduled run as what.
func (s *Server) readable(r *http.Request, id, what string) bool {
	if s.mayAccess(r, id) {
		return true
	}
	rec, ok := s.scheduledForAdmin(r, id)
	if ok {
		s.adminAudit(r, "session.read", id, map[string]any{"owner": rec.User, "via": what})
	}
	return ok
}

// sessionGetter is a store that finds one session row by id.
type sessionGetter interface {
	GetSession(ctx context.Context, id string) (store.SessionRecord, error)
}

// ownerLister is a store that can list one owner's sessions in its query.
type ownerLister interface {
	ListSessionsOwnedBy(ctx context.Context, owner string, limit int) ([]store.SessionRecord, error)
}

// listOwned lists the newest sessions user may see. The owner is filtered in
// the store where it can be, so the tenant's newest do not crowd theirs out.
func (s *Server) listOwned(ctx context.Context, user string, limit int) ([]store.SessionRecord, error) {
	if ol, ok := s.sessions.(ownerLister); ok && user != "" && user != auth.Anonymous && !auth.OwnsNothing(user) {
		return ol.ListSessionsOwnedBy(ctx, user, limit)
	}
	return s.sessions.ListSessions(ctx, limit)
}

// ownsStored reports whether the caller owns a session this node is not
// running, found by id where the store can, so an older session is not missed.
func (s *Server) ownsStored(r *http.Request, id string) bool {
	g, ok := s.sessions.(sessionGetter)
	if !ok {
		return s.mayAccess(r, id)
	}
	rec, err := g.GetSession(r.Context(), id)
	if err != nil {
		return false
	}
	return ownsSession(rec.Tenant, rec.User, TenantOf(r.Context()), UserOf(r.Context()))
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
	if recTenant != tenant || auth.OwnsNothing(user) {
		return false
	}
	if user == "" || user == auth.Anonymous {
		return true
	}
	return recUser == user
}

// streamEnd finds the end after which a session makes no more events: one that
// owes no background work, and whose suggestion, if any, has its model.call.
// A suggestion an end announces is awaited across the settled end that may
// follow it, which can come before the suggestion's events.
type streamEnd struct{ suggestion, owed bool }

// closes reports whether the stream ends after e; running reports a run live now.
func (st *streamEnd) closes(e agent.Event, running func() bool) bool {
	if st.suggestion && e.Type == agent.EvModelCall {
		var c agent.ModelCall
		if json.Unmarshal(e.Payload, &c) == nil && c.Purpose == agent.PurposeSuggestion {
			st.suggestion = false
			// A prompt sent meanwhile has started the next run: it streams on.
			return !st.owed && !running()
		}
	}
	if e.Type != agent.EvSessionEnded {
		return false
	}
	var end agent.SessionEnded
	if json.Unmarshal(e.Payload, &end) != nil {
		return true
	}
	st.owed = end.Background > 0
	// A run's own end says whether a suggestion follows it; the settled end only adds one.
	if end.Settled {
		st.suggestion = st.suggestion || end.Suggesting
	} else {
		st.suggestion = end.Suggesting
	}
	return !st.owed && !st.suggestion
}

// streamEvents serves the session's event stream over SSE, resumable via
// Last-Event-ID so a dropped connection does not lose the session.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// A session this process is not running may still be replayable from a
	// durable store — that is the whole point of event sourcing. Looking only
	// at the in-memory map meant every session from before a restart returned
	// 404, so clicking one in the UI showed a blank pane.
	live, running := s.session(r.Context(), id, TenantOf(r.Context()), UserOf(r.Context()))
	if !running {
		// Not running is not the same as not ours. Ownership is checked
		// against the stored record before any event is streamed, or a
		// finished session becomes readable by anyone who knows its id.
		if !s.readable(r, id, "events") {
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

	// Subscribe before reading the backlog: an event recorded between the
	// two is then in both, and send skips what was already written. Read the
	// other way round, an event recorded in that gap reached neither, and a
	// quiet session never showed it.
	var events <-chan agent.Event
	if running {
		events = s.store.Subscribe(live.ID)
		defer s.store.Unsubscribe(live.ID, events)
	}

	// The backlog never closes the stream, but a suggestion it shows owed is awaited.
	var end streamEnd
	runLive := func() bool {
		live.mu.Lock()
		defer live.mu.Unlock()
		return live.State == "running"
	}
	if backlog, err := s.store.Since(id, lastSeq); err == nil {
		for _, ev := range backlog {
			writeSSE(w, ev)
			lastSeq = ev.Seq
			if running {
				end.closes(ev, runLive)
			}
		}
		flusher.Flush()
	}

	// Only a session still running in this process can produce new events.
	// For a replayed one the backlog above is the whole story, so close cleanly
	// rather than holding a connection open that will never deliver anything.
	if !running {
		return
	}

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	// Authorised again while it runs, not only when it opened: every write
	// below goes through guard.allowed, and a refusal ends the stream.
	guard := s.guardReadStream(r, id)
	defer guard.stop()

	// The store drops events for a subscriber that falls behind rather than
	// stall the loop. Seeing a full buffer, or a gap in seq, means some may
	// be gone, and they are read back from the record before going on.
	// ended is true once the stream is over: the session ended or the
	// caller is no longer authorised to read it.
	send := func(batch []agent.Event) (ended bool) {
		for _, e := range batch {
			if e.Seq <= lastSeq {
				continue
			}
			// Per event, not per batch: a refill can be a whole backlog, and a
			// recheck asked for midway must stop the rest of it.
			if err := guard.allowed(); err != nil {
				flusher.Flush()
				endStream(w, err)
				return true
			}
			lastSeq = e.Seq
			writeSSE(w, e)
			ended = ended || end.closes(e, runLive)
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
			if err := guard.check(); err != nil {
				endStream(w, err)
				return
			}
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-guard.tick():
			if err := guard.check(); err != nil {
				endStream(w, err)
				return
			}
		case <-guard.woken():
			if err := guard.allowed(); err != nil {
				endStream(w, err)
				return
			}
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
	if !s.readable(r, id, "replay") {
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
	if !s.readable(r, id, "hawkeye") {
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
	// Live here whoever owns it: mayAccess has already admitted the caller.
	s.mu.RLock()
	_, live := s.running[id]
	s.mu.RUnlock()
	rep := hawkeye.AnalyzeWith(id, events, hawkeye.Options{Live: live})
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
		badBody(w, err, "invalid JSON body")
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

	live, ok := s.session(r.Context(), id, TenantOf(r.Context()), UserOf(r.Context()))
	resumedHere := false
	if !ok {
		// Not running here is not the end of it. A finished session is
		// continued from its record — by this process after a restart, or by
		// another node entirely — provided the caller owns it.
		if !validSessionID(id) || !s.mayAccess(r, id) {
			WriteError(w, http.StatusNotFound, "session not found")
			return
		}
		resumed, err := s.resumeSession(r.Context(), id, req.Prompt, UserOf(r.Context()), TenantOf(r.Context()), true)
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
		resumedHere = true
	}
	// Held from the claim until the turn is running, so a workbench hold
	// cannot be released between them and end the session a second time.
	live.claimMu.Lock()
	claimedHere, started := resumedHere, false
	defer func() {
		// A claim this request took and did not use is given back as it was found.
		if claimedHere && !started {
			s.giveBack(id, live)
		}
		live.claimMu.Unlock()
	}()
	// Checked before claiming: a claim taken during a drain would never end.
	if s.draining.Load() {
		w.Header().Set("Retry-After", "5")
		WriteError(w, http.StatusServiceUnavailable, "server is shutting down; retry")
		return
	}
	newly, err := s.claimLocked(r.Context(), id, live)
	switch {
	case errors.Is(err, errBusySession):
		WriteError(w, http.StatusConflict, "session is being continued elsewhere")
		return
	case errors.Is(err, errHoldFailed):
		writeHoldFailed(w)
		return
	case err != nil:
		s.log.Error("claim failed", "session", id, "error", err)
		WriteError(w, http.StatusInternalServerError, "could not continue the session")
		return
	}
	claimedHere = claimedHere || newly
	if live.fallback != nil {
		// The record says why the next call names another model.
		if _, err := live.Loop.Recorder.Record(agent.EvModelSwitched, agent.ActorSystem, agent.Trusted, *live.fallback); err != nil {
			s.log.Error("model fallback not recorded", "session", id, "error", err)
			WriteError(w, http.StatusInternalServerError, "could not continue the session: its change of model could not be recorded")
			return
		}
		live.fallback = nil
	}

	// The hold, with its liveness, comes before the run: a process that
	// cannot record that it holds the session does not run it. A message
	// that only steers a live run gives nothing up.
	if err := s.holdNode(live); err != nil {
		writeHoldFailed(w)
		return
	}
	defer func() {
		if !started {
			s.releaseNodeIfQuiet(live)
		}
	}()
	live.mu.Lock()
	// While draining, nothing is started, steered or interrupted: a steering
	// message could be dropped, and Send now would stop the turn the drain
	// is letting finish. Checked before anything touches the turn.
	if s.draining.Load() {
		live.mu.Unlock()
		w.Header().Set("Retry-After", "5")
		WriteError(w, http.StatusServiceUnavailable, "server is shutting down; retry")
		return
	}
	// The owner's own admitted turn shows them active again, so wakes and
	// suggestions a revoke held come back for a restored owner.
	if UserOf(r.Context()) == live.User {
		live.ownerGone.Store(false)
	}
	// Busy means a run is live, not an ask pending: an ask can wait with no
	// run, and a message queued then would reach no loop that runs it.
	busy := live.ran != nil
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
		if live.ran != nil {
			live.mu.Unlock()
			WriteError(w, http.StatusConflict, "another turn started first")
			return
		}
	}
	// Checked under the session's lock: a shutdown cancels what it finds
	// under the same lock, so no turn can start unseen once draining began.
	if s.draining.Load() {
		live.mu.Unlock()
		w.Header().Set("Retry-After", "5")
		WriteError(w, http.StatusServiceUnavailable, "server is shutting down; retry")
		return
	}
	live.Turns++
	if live.Prompt == "" {
		live.Prompt = req.Prompt // a workbench session is named by its first message
	}
	s.startRunLocked(live, "follow-up", func(ctx context.Context) (agent.TerminalReason, error) {
		return live.Loop.RunMessage(ctx, msg)
	})
	started = true
	WriteJSON(w, http.StatusAccepted, map[string]string{"session_id": id})
}

// startRunLocked starts a run on its own goroutine: a person's message or a
// wake. The caller holds live.mu, has checked no run is live and the server
// is not draining, and has the session claimed; this releases live.mu. One
// path, so every run keeps the node's claim and settles the same way.
func (s *Server) startRunLocked(live *liveSession, what string, start func(ctx context.Context) (agent.TerminalReason, error)) {
	live.takeOver()
	live.State = "running"
	live.ran = make(chan struct{})
	ctx, cancelCause := context.WithCancelCause(context.Background())
	cancel := func() { cancelCause(nil) }
	live.cancel = cancel
	live.cancelCause = cancelCause
	live.mu.Unlock()

	go func() {
		defer cancel()
		live.undo.BeginTurn()
		reason, err := start(ctx)
		for live.settle(ctx, reason, err) {
			reason, err = live.Loop.RunQueued(ctx)
		}
		waitSuggestion(live)
		s.releaseAndLetGo(live)
		switch {
		case errors.Is(err, agent.ErrNothingToWake):
			s.log.Info(what+" found nothing to do", "session", live.ID)
		case err != nil:
			s.log.Error(what+" failed", "session", live.ID, "error", err)
		default:
			s.log.Info(what+" ended", "session", live.ID, "reason", reason)
		}
	}()
}

// settle ends a run, unless a message was queued after the loop last looked
// and the run ended cleanly: then it reports true and the caller runs again.
func (l *liveSession) settle(ctx context.Context, reason agent.TerminalReason, err error) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A wake that found nothing to do (its results taken, or a Stop since)
	// is no failure: the session keeps the end it had.
	if errors.Is(err, agent.ErrNothingToWake) {
		reason, err = l.Reason, nil
	}
	// A wake run stopped at its cap leaves a message queued after it last
	// looked as a completed run does: it runs now.
	if err == nil && ctx.Err() == nil && (reason == agent.TermCompleted || reason == agent.TermWakeLimit) && len(l.Loop.Queued()) > 0 {
		return true
	}
	l.State = "done"
	// Children still running, or a result still owed, keep the session
	// going and its stream open.
	if l.Loop.Background.Owed() > 0 {
		l.State = "background"
	}
	l.Reason = reason
	if err != nil && reason == "" {
		l.Reason = agent.TermError
	}
	if l.ran != nil {
		close(l.ran)
		l.ran = nil
	}
	return false
}

// onIdle takes what a session's background work did while no run was live:
// once it has all ended the session is done, and this node lets it go.
func (s *Server) onIdle(live *liveSession, ev agent.IdleEvent) {
	if !ev.Settled {
		return
	}
	live.mu.Lock()
	if live.ran == nil && live.State == "background" {
		live.State = "done"
	}
	live.mu.Unlock()
	s.releaseAndLetGo(live)
}

// recoverOrphan takes over a session left open by a process that went away,
// when the store can say no live node holds it, and reconciles its record.
func (s *Server) recoverOrphan(ctx context.Context, id string, events []agent.Event) bool {
	oc, ok := s.sessions.(OrphanClaimer)
	if !ok || !agent.Orphaned(events) {
		return false
	}
	claimed, err := oc.ClaimOrphan(ctx, id, s.holder, nodeStale)
	if err != nil || !claimed {
		return false
	}
	return s.reconcile(id, events)
}

// reconcile writes what a claimed orphan's crashed holder never did.
func (s *Server) reconcile(id string, events []agent.Event) bool {
	if err := agent.Reconcile(s.store, id, events); err != nil {
		s.log.Error("could not reconcile an orphaned session", "session", id, "error", err)
		return false
	}
	s.log.Warn("recovered a session a crashed process left open", "session", id)
	return true
}

// RecoverOrphans reconciles the open sessions whose holder's heartbeat has
// gone stale: a crashed process's lost tasks are recorded as lost and its
// ends written, so the list shows them ended and continuable. A session
// whose holder is alive, here or elsewhere, is left alone. It reports how
// many it reconciled.
func (s *Server) RecoverOrphans(ctx context.Context) int {
	if s.sessions == nil {
		return 0
	}
	if _, ok := s.sessions.(OrphanClaimer); !ok {
		return 0
	}
	n := 0
	err := s.eachOpenSession(ctx, func(id string) {
		s.mu.RLock()
		_, here := s.running[id]
		s.mu.RUnlock()
		if here || ctx.Err() != nil {
			return
		}
		events, err := s.store.Events(id)
		if err != nil {
			return
		}
		if s.recoverOrphan(ctx, id, events) {
			s.releaseNode(id) // reconciled and ended: nothing of it runs here
			n++
		}
	})
	if err != nil {
		s.log.Warn("could not list sessions to recover", "error", err)
	}
	if n > 0 {
		s.log.Warn("recovered sessions a crashed process left open", "count", n)
	}
	return n
}

// sweepPage is how many open sessions the sweep reads at a time.
var sweepPage = 500

// OpenSessionLister is implemented by stores that can page through every
// session not yet ended, however old, for the orphan sweep.
type OpenSessionLister interface {
	OpenSessions(ctx context.Context, after string, limit int) ([]string, error)
}

// eachOpenSession calls f for every open session: through OpenSessions page
// by page where the store has it, otherwise from its session list.
func (s *Server) eachOpenSession(ctx context.Context, f func(id string)) error {
	if l, ok := s.sessions.(OpenSessionLister); ok {
		after := ""
		for ctx.Err() == nil {
			ids, err := l.OpenSessions(ctx, after, sweepPage)
			if err != nil {
				return err
			}
			for _, id := range ids {
				f(id)
			}
			if len(ids) < sweepPage {
				return nil
			}
			after = ids[len(ids)-1]
		}
		return ctx.Err()
	}
	recs, err := s.sessions.ListSessions(ctx, 500)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.EndedAt == nil {
			f(rec.ID)
		}
	}
	return nil
}

// OwnReclaimer is implemented by stores that let a node take back, at start,
// the open sessions still held under its own id.
type OwnReclaimer interface {
	ReclaimOwn(ctx context.Context, sessionID, holder string) (bool, error)
}

// RecoverOwnAtStart is the startup sweep, run before the server serves:
// with a node id, the open sessions held under it are this node's from
// before a restart, and none can be running here yet, so they are taken
// back at once; then the stale ones are recovered as RecoverOrphans does.
// It must not run while sessions can start: a session this process is
// starting is held under the same id.
func (s *Server) RecoverOwnAtStart(ctx context.Context) int {
	n := 0
	if r, ok := s.sessions.(OwnReclaimer); ok && s.opts.NodeID != "" {
		_ = s.eachOpenSession(ctx, func(id string) {
			events, err := s.store.Events(id)
			if err != nil || !agent.Orphaned(events) {
				return
			}
			if mine, err := r.ReclaimOwn(ctx, id, s.holder); err == nil && mine && s.reconcile(id, events) {
				s.releaseNode(id)
				n++
			}
		})
	}
	return n + s.RecoverOrphans(ctx)
}

// sweepOrphans runs RecoverOrphans every interval until ctx
// ends: a session whose holder goes stale later is recovered too.
func (s *Server) sweepOrphans(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.RecoverOrphans(ctx)
	}
}

// canWake says whether this server may start a wake run for the session now.
// An owner who is no longer active cannot answer its asks, so their
// session's children are cancelled rather than left to wait.
func (s *Server) canWake(live *liveSession) (bool, string) {
	switch {
	case s.draining.Load():
		return false, "draining"
	case live.unclaimed.Load():
		return false, "not_claimed"
	case live.ownerGone.Load():
		return false, "owner_inactive"
	}
	return true, ""
}

// checkOwner looks the session's owner up before an idle delivery, outside
// the run lock. An owner no longer active cannot answer their children's
// asks, so those children are cancelled whatever the wake mode, and no wake
// runs for them.
func (s *Server) checkOwner(live *liveSession) {
	gone := !s.ownerActive(live)
	live.ownerGone.Store(gone)
	if gone {
		live.Loop.Background.CancelAll(agent.TermOwnerInactive)
	}
}

// StopOwnerBackground stops what this process runs for an owner whose access
// was withdrawn: each of their sessions' live run, background shells and
// tasks, and terminals, recorded as owner_revoked; and no wake starts for
// those sessions until the owner is found active again. An empty tenant
// matches every tenant; sessions already released from the owner (see
// ReleaseSessions) are matched too. It returns how many it stopped. ctx
// bounds the wait for the runs to record their end.
func (s *Server) StopOwnerBackground(ctx context.Context, tenant, user string) int {
	if user == "" || user == auth.Anonymous || auth.OwnsNothing(user) {
		return 0
	}
	released := auth.UnclaimedOwner(user)
	s.mu.RLock()
	var hit []*liveSession
	for _, live := range s.running {
		if (tenant == "" || live.Tenant == tenant) && (live.User == user || live.User == released) {
			hit = append(hit, live)
		}
	}
	s.mu.RUnlock()
	n := 0
	ran := make([]chan struct{}, len(hit))
	for i, live := range hit {
		live.ownerGone.Store(true)
		stopped := 0
		live.mu.Lock()
		stop, run := live.cancelCause, live.ran
		live.mu.Unlock()
		if stop != nil && run != nil {
			stop(agent.StopCause{Reason: agent.TermOwnerRevoked})
			ran[i] = run
			stopped++
		}
		if live.Loop != nil {
			stopped += live.Loop.Background.CancelAll(agent.TermOwnerRevoked)
		}
		stopped += len(live.closeTerminals(string(agent.TermOwnerRevoked)))
		if stopped > 0 {
			s.log.Warn("stopped an owner's work", "session", live.ID, "owner", user,
				"stopped", stopped, "reason", "owner access revoked")
		}
		n += stopped
	}
	// One deadline for every run, waited in parallel: a call returns within
	// about turnEndWait plus the 2s a suggestion is given to stop.
	wctx, cancel := context.WithTimeout(ctx, turnEndWait)
	defer cancel()
	var wg sync.WaitGroup
	for i, live := range hit {
		wg.Add(1)
		go func(live *liveSession, run chan struct{}) {
			defer wg.Done()
			if run != nil {
				select {
				case <-run:
				case <-wctx.Done():
				}
			}
			// After the run: a suggestion it started as it ended is stopped too,
			// so no model call is made for the revoked owner.
			if live.Loop != nil {
				live.Loop.StopSuggestion()
			}
		}(live, ran[i])
	}
	wg.Wait()
	return n
}

// ownerActive asks Options.OwnerActive, or the local accounts when there
// are any: the owner's account must still exist. live.User is an owner key.
func (s *Server) ownerActive(live *liveSession) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.opts.OwnerActive != nil {
		return s.opts.OwnerActive(ctx, live.Tenant, live.User)
	}
	local := s.LocalAuth()
	if local == nil {
		return true
	}
	users, err := local.Store.List(ctx)
	if err != nil {
		return false // unknown is not active: no run starts on their behalf
	}
	for _, u := range users {
		// Sessions are owned by the owner key, as Identity.Owner makes it.
		if auth.LocalOwner(u.Username) == live.User {
			return true
		}
	}
	return false
}

// wake starts a wake run for background results, through the same start
// path as a message, so draining, the claim and steering hold as they do
// for one. It reports false when a run is live or starting one is refused.
func (s *Server) wake(live *liveSession, ids []string) bool {
	live.claimMu.Lock()
	defer live.claimMu.Unlock()
	if err := s.holdNode(live); err != nil {
		return false
	}
	live.mu.Lock()
	if live.ran != nil || s.draining.Load() || live.unclaimed.Load() {
		live.mu.Unlock()
		s.releaseNodeIfQuiet(live)
		return false
	}
	s.startRunLocked(live, "wake", func(ctx context.Context) (agent.TerminalReason, error) {
		return live.Loop.RunWoken(ctx, agent.Wake{By: "policy", TaskIDs: ids})
	})
	return true
}

// holdNode keeps this node's claim on the session fresh while it has a run
// or a background child live, so routing and answers from another node
// find it; once for each stretch of activity.
func (s *Server) holdNode(live *liveSession) error {
	live.mu.Lock()
	defer live.mu.Unlock()
	if live.beatStop != nil {
		return nil
	}
	if err := s.claimNode(context.Background(), live.ID); err != nil {
		return err
	}
	live.beatStop = s.heartbeatNode(context.Background(), live.ID, func() { s.fence(live) })
	return nil
}

// fence stops everything this process runs for a session whose claim it has
// lost: nothing more is written to the session's record, the run and the
// background tasks end as lease_lost (recorded in their own records), and
// the session leaves this process. The claim is not released: it is
// another's now.
func (s *Server) fence(live *liveSession) {
	if !live.fenced.CompareAndSwap(false, true) {
		return
	}
	s.log.Error("lost the claim on a session to another process; stopping it here", "session", live.ID)
	live.mu.Lock()
	stop, cancelCause := live.beatStop, live.cancelCause
	live.beatStop = nil
	live.mu.Unlock()
	if stop != nil {
		stop()
	}
	s.mu.Lock()
	if s.running[live.ID] == live {
		delete(s.running, live.ID)
	}
	s.mu.Unlock()
	if cancelCause != nil {
		cancelCause(agent.StopCause{Reason: agent.TermLeaseLost})
	}
	live.closeTerminals(closedWithSession)
	live.Loop.Background.Close(agent.TermLeaseLost)
}

// releaseNodeNow ends the hold whatever is live, for a session going away
// (deleted, or the process draining).
func (s *Server) releaseNodeNow(live *liveSession) {
	live.mu.Lock()
	stop := live.beatStop
	live.beatStop = nil
	live.mu.Unlock()
	if stop != nil {
		stop()
		s.releaseNode(live.ID)
	}
}

// releaseNodeIfQuiet gives the claim up once no run, no child and no
// workbench hold is live.
func (s *Server) releaseNodeIfQuiet(live *liveSession) {
	live.holdMu.Lock()
	held := live.held
	live.holdMu.Unlock()
	live.mu.Lock()
	stop := live.beatStop
	quiet := stop != nil && !held && live.ran == nil && live.Loop.Background.Owed() == 0
	if quiet {
		live.beatStop = nil
	}
	live.mu.Unlock()
	if quiet {
		stop()
		s.releaseNode(live.ID)
	}
}

// releaseAndLetGo is releaseNodeIfQuiet at the end of a run or of background
// work, where the caller holds no claim lock. On a store that fences writes
// on the claim, a session whose row has ended is also let go: marked
// unclaimed, with the end it has now to go back to, before its claim is
// released, so its next write claims it again. Both happen under claimMu,
// so no message starts a run between them.
func (s *Server) releaseAndLetGo(live *liveSession) {
	if _, fenced := s.under().(*store.Held); !fenced || live.fenced.Load() || live.unclaimed.Load() {
		s.releaseNodeIfQuiet(live)
		return
	}
	events, err := s.store.Events(live.ID)
	if end, ok := agent.LastEnd(events); err != nil || !ok || end.Background > 0 {
		s.releaseNodeIfQuiet(live) // the row is still open: nothing to claim it back from
		return
	}
	live.claimMu.Lock()
	defer live.claimMu.Unlock()
	live.holdMu.Lock()
	held := live.held
	live.holdMu.Unlock()
	live.mu.Lock()
	idle := live.ran == nil && live.Loop.Background.Owed() == 0
	live.mu.Unlock()
	if idle && !held {
		live.priorEnd = priorEnd(events, store.SessionRecord{})
		live.unclaimed.Store(true)
	}
	s.releaseNodeIfQuiet(live)
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
	live, ok := s.session(r.Context(), r.PathValue("id"), TenantOf(r.Context()), UserOf(r.Context()))
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
	live, ok := s.session(r.Context(), r.PathValue("id"), TenantOf(r.Context()), UserOf(r.Context()))
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

// listTasks answers the owner with the session's background tasks.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	live, ok := s.session(r.Context(), r.PathValue("id"), TenantOf(r.Context()), UserOf(r.Context()))
	if !ok {
		s.notRunningHere(w, r, r.PathValue("id"))
		return
	}
	tasks := live.Loop.Background.Tasks()
	if tasks == nil {
		tasks = []agent.TaskInfo{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "wake": live.Loop.Background.Mode()})
}

// cancelTask stops one of the session's background tasks for its owner, as
// a stop by the person.
func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	live, ok := s.session(r.Context(), r.PathValue("id"), TenantOf(r.Context()), UserOf(r.Context()))
	if !ok {
		s.notRunningHere(w, r, r.PathValue("id"))
		return
	}
	if !live.Loop.Background.Cancel(r.PathValue("task"), agent.TermUserInterrupt) {
		WriteError(w, http.StatusNotFound, "no such running task in this session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setWake is the owner's switch for what a background result does while
// the session is idle, up to what this server allows.
func (s *Server) setWake(w http.ResponseWriter, r *http.Request) {
	live, ok := s.session(r.Context(), r.PathValue("id"), TenantOf(r.Context()), UserOf(r.Context()))
	if !ok {
		s.notRunningHere(w, r, r.PathValue("id"))
		return
	}
	var req struct {
		Wake string `json:"wake"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badBody(w, err, "invalid JSON body")
		return
	}
	if err := live.Loop.Background.SetWake(agent.WakeMode(req.Wake), agent.ByUser); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"wake": req.Wake})
}

func (s *Server) interruptSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	live, ok := s.session(r.Context(), id, TenantOf(r.Context()), UserOf(r.Context()))
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
	// Stop means stop: every background child too, running or idle.
	live.Loop.Background.CancelAll(agent.TermUserInterrupt)
	w.WriteHeader(http.StatusNoContent)
}

type approveRequest struct {
	Approved bool   `json:"approved"`
	Scope    string `json:"scope,omitempty"`
	// RequestID is the id of the action.requested event being answered.
	// Optional for older clients, which answer whichever request is pending;
	// clients should send it.
	RequestID string `json:"request_id,omitempty"`
}

func (s *Server) approveAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req approveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badBody(w, err, "invalid JSON body")
		return
	}
	live, ok := s.session(r.Context(), id, TenantOf(r.Context()), UserOf(r.Context()))
	if !ok {
		// The session is not here. If the store holds the approval, answer it
		// anyway: the waiting node polls for the result, so the reviewer's
		// decision still arrives. This is the case sticky routing exists to
		// avoid and the one durability exists to survive when it does not.
		if s.answerElsewhere(w, r, id, req) {
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
	s.answerHere(w, r, live, req)
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
	// The pages sign out by fetch and then go where the provider's redirect points.
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		c := &redirectCapture{ResponseWriter: w}
		s.endSignIn(c, r)
		if c.code == 0 {
			return
		}
		WriteJSON(w, http.StatusOK, map[string]string{"next": c.loc})
		return
	}
	s.endSignIn(w, r)
}

// redirectCapture holds back a provider's sign-out redirect so a fetch can be
// told where to go instead of following it.
type redirectCapture struct {
	http.ResponseWriter
	loc  string
	code int
}

func (c *redirectCapture) WriteHeader(code int) {
	if code >= 300 && code < 400 && c.code == 0 {
		c.code, c.loc = code, c.Header().Get("Location")
		c.Header().Del("Location")
		c.Header().Del("Content-Type")
		return
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *redirectCapture) Write(b []byte) (int, error) {
	if c.code != 0 {
		return len(b), nil
	}
	return c.ResponseWriter.Write(b)
}

func (s *Server) endSignIn(w http.ResponseWriter, r *http.Request) {
	// The local session the cookie names ends even when its account cannot be read.
	if l := s.LocalAuth(); l != nil && l.HasSession(r) {
		l.SignOut(w, r)
		return
	}
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
	id, mode, err := s.authMiddleware().Identify(w, r)
	if id == nil {
		out := map[string]any{
			"authenticated": false,
			"auth_mode":     orDefaultStr(s.opts.Config.Auth.Mode, "none"),
		}
		if err != nil {
			out["reason"] = err.Error()
		}
		WriteJSON(w, http.StatusOK, out)
		return
	}
	me := map[string]any{
		"authenticated": true, "auth_mode": mode,
		"subject": id.Subject, "email": id.Email, "name": id.Name,
		"tenant": id.Tenant, "groups": id.Groups, "owner": id.Owner(),
		"sign_out_url": "/logout",
	}
	// Switching user is a fresh sign-in: the identity provider's own
	// prompt when there is one, the sign-in page for local accounts.
	switch mode {
	case "local":
		me["switch_url"], me["password_url"] = "/logout", "/account"
		if local := s.LocalAuth(); local != nil && local.MustChangePassword(r) {
			me["must_change_password"] = true
		}
	case "oidc":
		me["switch_url"] = "/switch-user"
	case "proxy":
		// The proxy owns sign-in, so there is nothing to switch to here, and
		// sign-out exists only where the operator named the proxy's own.
		delete(me, "sign_out_url")
		if u := s.opts.Config.Auth.ProxyLogoutURL; u != "" {
			me["sign_out_url"] = u
		}
	}
	WriteJSON(w, http.StatusOK, me)
}

// mustChangeGate confines a session whose password was set for it to the
// routes that change it, until it has been changed.
func (s *Server) mustChangeGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		local := s.LocalAuth()
		if local == nil || mustChangeAllowed(r) || !local.MustChangePassword(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/account?must_change=1", http.StatusFound)
			return
		}
		WriteError(w, http.StatusForbidden, "password change required")
	})
}

// mustChangeAllowed names what a must-change session may still reach.
func mustChangeAllowed(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == "/account", p == "/v1/whoami", p == "/logout", p == "/v1/health",
		p == "/favicon.ico", p == "/favicon.svg", strings.HasPrefix(p, "/ide/vendor/"):
		return true
	case r.Method == http.MethodPost && (p == "/v1/password" || p == "/v1/signin"):
		// Signing in again, as someone else, replaces the session outright.
		return true
	}
	return false
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
		badBody(w, err, "invalid request")
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
		// Checked before the code is spent, so a taken name or a short
		// password does not use up the invite.
		if err := local.CheckNewUser(r.Context(), req.Username, req.Password); err != nil {
			WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := local.CheckEmail(r.Context(), req.Username, strings.TrimSpace(req.Email)); err != nil {
			WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if c, ok := s.opts.Invites.(InviteEmailChecker); ok {
			if err := c.CheckInviteEmail(r.Context(), req.Invite, strings.TrimSpace(req.Email)); err != nil {
				WriteJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
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
	WebFetch   bool     `json:"web_fetch"`
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
	o.WebFetch = cfg.WebFetch.Enabled

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
	}
	if id, _, _ := s.authMiddleware().Identify(w, r); id != nil {
		o.Authenticated = true
		o.User = orDefaultStr(id.Email, id.Subject)
		o.Admin = slices.Contains(id.Groups, s.adminGroup())
		o.Tenant = id.Tenant
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
	if pg, ok := s.under().(interface {
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
	// draining lets the workbench leave a queued message where it is rather
	// than withdraw it for a Send now the server would refuse.
	WriteJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"sessions": n,
		"model":    s.opts.Adapter.Profile().Name,
		"draining": s.draining.Load(),
	})
}

// claimNode records that this process holds the session, under its liveness
// identity, wherever the store keeps holders, with or without a NodeID. A
// failure is returned: it is also the liveness another process reads to
// decide whether the session was left behind by a crash.
func (s *Server) claimNode(ctx context.Context, sessionID string) error {
	r, ok := s.liveness()
	if !ok {
		return nil
	}
	if err := r.ClaimNode(ctx, sessionID, s.holder); err != nil {
		s.log.Warn("could not claim session for this process", "session", sessionID, "err", err)
		return err
	}
	return nil
}

// liveness is the store's holder bookkeeping, used whether or not routing by
// node is configured.
func (s *Server) liveness() (SessionRouter, bool) {
	r, ok := s.under().(SessionRouter)
	return r, ok
}

// holderID is the process's liveness identity: the node id when configured,
// with a token of this incarnation after a '#', otherwise one of its own. A
// restarted node, or two processes given one node id, hold leases apart, so
// a fenced append tells them apart; routing reads the node id before the '#'.
func holderID(nodeID string) string {
	if nodeID != "" {
		return nodeID + "#" + newSessionID()
	}
	return "instance-" + newSessionID()
}

// heartbeatNode keeps this node's claim on the session fresh while the turn
// runs. Without it a claim ages out of the staleness window and a healthy node
// stops being found, so the turns most likely to need an approval — the long
// ones — are exactly the ones whose approvals get misrouted.
//
// The returned function stops the heartbeat; it is safe to call more than once.
func (s *Server) heartbeatNode(ctx context.Context, sessionID string, lost func()) func() {
	return s.heartbeatNodeEvery(ctx, sessionID, nodeHeartbeat, lost)
}

// heartbeatNodeEvery refreshes the claim every interval. A renewal the store
// refuses (another process holds the session now), or failures lasting
// until the claim would read as stale to others, mean the lease is lost:
// lost runs, once, and the heartbeat ends.
func (s *Server) heartbeatNodeEvery(ctx context.Context, sessionID string, every time.Duration, lost func()) func() {
	if _, ok := s.liveness(); !ok {
		return func() {}
	}
	ctx, stop := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		lastOK := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Its own context: the run's may be seconds from cancellation,
				// and a refresh that fails then would look like a dead node.
				beat, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				// The claim is fresh from when the renewal was sent, not when
				// it was answered: a slow answer does not stretch the budget.
				sent := time.Now()
				held, err := s.renewNode(beat, sessionID)
				cancel()
				switch {
				case err == nil && held:
					lastOK = sent
					continue
				case err != nil && time.Since(lastOK) < nodeStale-every:
					continue // logged; the next beat tries again while the claim still reads as live
				}
				if ctx.Err() == nil && lost != nil {
					lost()
				}
				return
			}
		}
	}()
	return stop
}

// renewNode refreshes this process's claim: fenced where the store can
// fence it, and otherwise by claiming again.
func (s *Server) renewNode(ctx context.Context, sessionID string) (bool, error) {
	if r, ok := s.under().(LeaseRenewer); ok {
		held, err := r.RenewNode(ctx, sessionID, s.holder)
		if err != nil {
			s.log.Warn("could not renew the claim on a session", "session", sessionID, "err", err)
		}
		return held, err
	}
	if err := s.claimNode(ctx, sessionID); err != nil {
		if errors.Is(err, store.ErrHeldElsewhere) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// releaseNode clears the claim when a turn finishes. It uses its own context:
// the run's context is cancelled by the time this is reached.
func (s *Server) releaseNode(sessionID string) {
	r, ok := s.liveness()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.ReleaseNode(ctx, sessionID, s.holder); err != nil {
		s.log.Warn("could not release session claim", "session", sessionID, "err", err)
	}
}

// answerElsewhere records a decision for a session this node is not running,
// and reports whether it did. The node that is waiting polls for the result,
// so the answer is delivered without the request ever reaching it.
func (s *Server) answerElsewhere(w http.ResponseWriter, r *http.Request, sessionID string, req approveRequest) bool {
	d := s.approvalStore()
	// The store's rows do not carry the request id, so an answer naming one
	// is left to the node running the session, which can check it.
	if d == nil || req.RequestID != "" {
		return false
	}
	// The same owner check the local path makes, against the stored row:
	// without it anyone signed in could answer another person's card.
	if !s.ownsStored(r, sessionID) {
		return false
	}
	pending, found, err := d.PendingApproval(r.Context(), sessionID)
	if err != nil || !found {
		return false
	}
	// Only a node running the session waits on the row. A row left by a
	// request that ended, or by a node that is gone, would be approved for
	// nothing, and read as approved in the record.
	if pending.Ended || s.elsewhere(r.Context(), sessionID) == "" {
		WriteError(w, http.StatusConflict, "that approval is no longer pending")
		return true
	}
	if req.Scope != "" && req.Scope != pending.Scope {
		WriteError(w, http.StatusBadRequest, "scope must be empty or the scope this request offered")
		return true
	}
	answered, err := d.AnswerApproval(r.Context(), pending.ID, req.Approved, req.Scope, UserOf(r.Context()))
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
	a, ok := s.under().(ApprovalStore)
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
	r, ok := s.under().(SessionRouter)
	return r, ok
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
// invented the ID, so the lookup does not confirm that a session exists. A
// held session whose stored owner has changed is released and not found.
func (s *Server) session(ctx context.Context, id, tenant, user string) (*liveSession, bool) {
	s.mu.RLock()
	live, found := s.running[id]
	owned := found && ownsSession(live.Tenant, live.User, tenant, user)
	s.mu.RUnlock()
	if !owned || !s.stillOwned(ctx, live, id) {
		return nil, false
	}
	return live, true
}

// stillOwnedWait bounds stillOwned's read of the session row.
const stillOwnedWait = 5 * time.Second

// stillOwned checks a held session's owner against its stored row, which an
// account's removal in another process moves to unclaimed while this one
// still holds the old owner. A disagreement adopts the row's owner. Without
// a durable store, or before the row exists, memory is all there is.
func (s *Server) stillOwned(ctx context.Context, live *liveSession, id string) bool {
	g, ok := s.sessions.(sessionGetter)
	if !ok {
		return true
	}
	s.mu.RLock()
	held, heldTenant := live.User, live.Tenant
	s.mu.RUnlock()
	if held == "" || held == auth.Anonymous {
		return true
	}
	// The request's own deadline, and at most a few seconds: a store that
	// does not answer in time refuses, as one that errs does.
	ctx, cancel := context.WithTimeout(ctx, stillOwnedWait)
	defer cancel()
	rec, err := g.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	if err != nil {
		// A store that cannot answer is not permission to proceed.
		return false
	}
	if rec.User == held && rec.Tenant == heldTenant {
		return true
	}
	s.mu.Lock()
	if live.User == held {
		live.User = rec.User
	}
	s.mu.Unlock()
	s.log.Warn("held session's owner no longer matches its record; released",
		"session", id, "held", held, "stored", rec.User)
	s.RecheckStreams()
	return false
}

// releaseStale lets go of the sessions this process holds for a local
// account that no longer exists, or exists again under the same name since
// they started: a later account of that name is not their owner.
func (s *Server) releaseStale(local *auth.LocalAuth, username string) {
	if username == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u, err := local.Store.Get(ctx, username)
	gone := errors.Is(err, auth.ErrNoSuchUser) || (err == nil && u == nil)
	if err != nil && !gone {
		return
	}
	owner := auth.LocalOwner(username)
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, live := range s.running {
		if live.User != owner || (!gone && (u.CreatedAt.IsZero() || !live.Created.Before(u.CreatedAt))) {
			continue
		}
		live.User = auth.UnclaimedOwner(owner)
		s.log.Warn("released a session whose account was removed or made again", "session", id, "owner", owner)
	}
}

// ReleaseSessions moves the sessions this process holds for owner in tenant
// to auth.UnclaimedOwner, as a removed account's rows are, and returns how many.
func (s *Server) ReleaseSessions(tenant, owner string) int {
	if owner == "" || owner == auth.Anonymous || auth.OwnsNothing(owner) {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, live := range s.running {
		if live.Tenant == tenant && live.User == owner {
			live.User = auth.UnclaimedOwner(owner)
			n++
		}
	}
	return n
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

// badBody answers a body that failed to decode: 413 when it was over the
// size cap, and otherwise 400 with msg.
func badBody(w http.ResponseWriter, err error, msg string) {
	if tooBig := new(http.MaxBytesError); errors.As(err, &tooBig) {
		WriteError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the request body is larger than %d bytes", tooBig.Limit))
		return
	}
	WriteError(w, http.StatusBadRequest, msg)
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
	// Sessions this node held before a restart are taken back, and sessions
	// a crashed process left open reconciled, before anything is served; then
	// again every stale window, by staleness alone.
	s.RecoverOwnAtStart(ctx)
	go s.sweepOrphans(ctx, nodeStale)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		s.drain()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			s.log.Warn("shutdown did not complete cleanly", "err", err)
		}
	}()
	s.log.Info("abhed server listening", "addr", s.opts.Addr, "workspace", s.opts.Workspace)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		// Serve returns as soon as Shutdown begins. Returning then would let
		// the process exit before cancelled turns record how they ended.
		<-stopped
	}
	return err
}

// drain stops this node taking new turns and gives the running ones until
// DrainTimeout to finish. What is still running when the budget runs out is
// cancelled with a stated cause, so it records as a shutdown rather than as a
// user interrupt and can be resumed on another node.
//
// With no budget configured this is the old behaviour: end everything at once.
func (s *Server) drain() {
	s.draining.Store(true)
	// Alongside the drain, so closing terminals adds nothing to the worst case.
	idleClosed := make(chan struct{})
	go func() { defer close(idleClosed); s.closeIdle() }()
	defer func() { <-idleClosed }()

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

// closeIdle ends the terminals of sessions opened with no prompt and marks
// their records ended, so a restarted server can reopen them from the record.
func (s *Server) closeIdle() {
	s.mu.Lock()
	var idle []*liveSession
	for _, live := range s.running {
		live.holdMu.Lock()
		held := live.held
		live.holdMu.Unlock()
		live.mu.Lock()
		if live.State == "idle" || held && live.State == "done" {
			idle = append(idle, live)
		}
		live.mu.Unlock()
	}
	s.mu.Unlock()
	// All at once, then the ends: a terminal's result is recorded as it
	// closes, and it belongs before the session's end, not after it.
	var closing []<-chan struct{}
	for _, live := range idle {
		closing = append(closing, live.closeTerminals(closedWithSession)...)
	}
	deadline := time.NewTimer(turnEndWait)
	defer deadline.Stop()
wait:
	for _, done := range closing {
		select {
		case <-done:
		case <-deadline.C:
			s.log.Warn("terminals did not record their end before shutdown")
			break wait
		}
	}
	for _, live := range idle {
		if live.unclaimed.Load() {
			continue // its row still holds the end it was opened with
		}
		live.holdMu.Lock()
		held := live.held
		live.holdMu.Unlock()
		if held {
			s.releaseHeld(live.ID, live, true) // workbench work alone: ended as it was opened
			continue
		}
		_, _ = live.Loop.Recorder.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted,
			agent.SessionEnded{Reason: agent.TermShutdown})
	}
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
		if state != "done" && state != "idle" {
			n++
		}
	}
	return n
}

// turnEndWait bounds how long a shutdown waits for cancelled turns to record
// their end, so a slow store cannot hold the process open.
const turnEndWait = 5 * time.Second

// suggestWait bounds how long a run's node waits for its suggestion to be
// recorded before letting the session go.
const suggestWait = 10 * time.Second

// waitSuggestion keeps the session on this node until the suggestion that
// follows its run's end is recorded, so that write is never a stranger's.
func waitSuggestion(live *liveSession) {
	ctx, cancel := context.WithTimeout(context.Background(), suggestWait)
	defer cancel()
	live.Loop.WaitSuggestion(ctx)
}

// cancelRunning ends every running turn as a shutdown and waits, up to
// turnEndWait, for each to record session.ended.
func (s *Server) cancelRunning() {
	s.mu.Lock()
	var ran []chan struct{}
	var withChildren []*liveSession
	for _, live := range s.running {
		if live.Loop != nil && live.Loop.Background.Owed() > 0 {
			withChildren = append(withChildren, live)
		}
		live.mu.Lock()
		if live.ran != nil {
			ran = append(ran, live.ran)
		}
		stop := live.cancelCause
		live.mu.Unlock()
		if stop != nil {
			stop(agent.ErrShutdown)
		}
	}
	s.mu.Unlock()
	// Children are goroutines holding model streams; they cannot move to
	// another node. They end as shutdown, and each session records its
	// closing end, so another node may claim it and rebuild their results.
	var closing sync.WaitGroup
	for _, live := range withChildren {
		closing.Add(1)
		go func() {
			defer closing.Done()
			live.Loop.Background.Close(agent.TermShutdown)
		}()
	}
	defer closing.Wait()
	deadline := time.NewTimer(turnEndWait)
	defer deadline.Stop()
	for _, ch := range ran {
		select {
		case <-ch:
		case <-deadline.C:
			s.log.Warn("turns did not record their end before shutdown")
			return
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
