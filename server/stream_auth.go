package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/zybuu-ai/abhed/auth"
)

// An event or terminal stream is one request that can stay open for hours.
// The middleware authorised it once, when it opened; without the guard below a
// user signed out, removed or refused by Check kept receiving everything the
// session did while each new request of theirs was turned away.

// maxStreamRecheck is the longest a stream trusts its last authorisation.
const maxStreamRecheck = 15 * time.Second

// defaultStreamRecheck is the interval when Options.StreamRecheck is unset.
const defaultStreamRecheck = 10 * time.Second

// streamCheckWait bounds one recheck, so a hung store stalls a stream for at
// most this long before it is ended.
const streamCheckWait = 5 * time.Second

// errStreamIdentity refuses a stream whose credentials now name someone else.
var errStreamIdentity = errors.New("the sign-in behind this stream has changed")

// errStreamSession refuses a stream whose caller no longer owns the session.
var errStreamSession = errors.New("session not found")

// errStreamMustChange refuses a stream whose account must change its password.
var errStreamMustChange = errors.New("password change required")

// streamRecheck is the interval streams are authorised again at.
func (s *Server) streamRecheck() time.Duration {
	d := s.opts.StreamRecheck
	if d <= 0 {
		d = defaultStreamRecheck
	}
	return min(d, maxStreamRecheck)
}

// streamGuard authorises one open stream again: on a timer, on each write
// once the last check is older than the interval, and at once when told a
// sign-in changed.
type streamGuard struct {
	s         *Server
	r         *http.Request
	sessionID string
	read      bool // a read-only stream, checked as mayRead
	user      string
	tenant    string
	every     time.Duration
	last      time.Time
	ticker    *time.Ticker
	// wake is signalled by RecheckStreams; stale makes the next write check
	// first, so nothing is written after that call returns unchecked.
	wake  chan struct{}
	stale chan struct{}
}

// guardStream registers a guard for r's stream of sessionID. The caller must
// call its stop when the stream ends.
func (s *Server) guardStream(r *http.Request, sessionID string) *streamGuard {
	return s.newStreamGuard(r, sessionID, false)
}

// guardReadStream is guardStream for a stream that only reads the record.
func (s *Server) guardReadStream(r *http.Request, sessionID string) *streamGuard {
	return s.newStreamGuard(r, sessionID, true)
}

func (s *Server) newStreamGuard(r *http.Request, sessionID string, read bool) *streamGuard {
	every := s.streamRecheck()
	g := &streamGuard{
		s: s, r: r, sessionID: sessionID, read: read,
		user: UserOf(r.Context()), tenant: TenantOf(r.Context()),
		every: every, last: time.Now(), ticker: time.NewTicker(every),
		wake: make(chan struct{}, 1), stale: make(chan struct{}, 1),
	}
	s.streamMu.Lock()
	if s.streams == nil {
		s.streams = map[*streamGuard]struct{}{}
	}
	s.streams[g] = struct{}{}
	s.streamMu.Unlock()
	return g
}

// atStreamStep runs the test hook for stage, if one is set.
func (s *Server) atStreamStep(stage string) {
	if s.streamStep != nil {
		s.streamStep(stage)
	}
}

func (g *streamGuard) stop() {
	g.ticker.Stop()
	g.s.streamMu.Lock()
	delete(g.s.streams, g)
	g.s.streamMu.Unlock()
}

// tick fires each interval, for a stream that has nothing to write.
func (g *streamGuard) tick() <-chan time.Time { return g.ticker.C }

// woken fires when a sign-in changed somewhere and this stream should check.
func (g *streamGuard) woken() <-chan struct{} { return g.wake }

// RecheckStreams has every open event and terminal stream authorise its
// caller again before it writes anything more. A sign-in layer calls it when
// it ends sessions or withdraws access; local accounts do so on their own.
func (s *Server) RecheckStreams() {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	for g := range s.streams {
		select {
		case g.stale <- struct{}{}:
		default:
		}
		select {
		case g.wake <- struct{}{}:
		default:
		}
	}
}

// allowed reports whether the stream may write now, checking first when the
// last check is older than the interval or a recheck was asked for.
func (g *streamGuard) allowed() error {
	select {
	case <-g.stale:
		return g.check()
	default:
	}
	if time.Since(g.last) >= g.every {
		return g.check()
	}
	return nil
}

// check authorises the stream again: the request's authentication rerun the
// way a new request's would be, then its hold on the session.
func (g *streamGuard) check() error {
	ctx, cancel := context.WithTimeout(g.r.Context(), streamCheckWait)
	defer cancel()
	id, err := auth.Recheck(ctx)
	if err != nil {
		return err
	}
	user, tenant := g.s.callerOf(ctx, id)
	if user != g.user || tenant != g.tenant {
		return errStreamIdentity
	}
	if local := g.s.LocalAuth(); local != nil && local.MustChangePassword(g.r) {
		return errStreamMustChange
	}
	if group := g.s.opts.Config.Auth.RequireGroup; group != "" && (id == nil || !slices.Contains(id.Groups, group)) {
		return notMember(group)
	}
	// Judged as the identity just rechecked: the one the stream opened with
	// may since have lost the admin group a read of a scheduled run needs.
	if id != nil {
		ctx = auth.WithIdentity(ctx, id)
	}
	may := g.s.mayAccess
	if g.read {
		may = g.s.mayRead
	}
	if !may(g.r.WithContext(ctx), g.sessionID) {
		return errStreamSession
	}
	g.last = time.Now()
	return nil
}

// endStream tells an SSE client why its stream is closing. The event is
// "refused" rather than "error", which an EventSource treats as a transport
// failure of its own.
func endStream(w http.ResponseWriter, err error) {
	b, _ := json.Marshal(map[string]string{"error": "access ended", "reason": err.Error()})
	fmt.Fprintf(w, "event: refused\ndata: %s\n\n", b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
