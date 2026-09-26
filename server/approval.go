package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/store"
)

// An approval request moves waiting -> answered -> taken, or to ended from
// waiting or answered. Every move is made under the session's lock by one
// helper, and both the turn's outcome and the HTTP reply follow from it.
type askState int

const (
	askWaiting  askState = iota
	askAnswered          // an answer is held for the turn, not yet taken
	askTaken             // the turn acted on an answer
	askEnded             // the turn stopped waiting without taking one
)

type pendingApproval struct {
	// RequestID is the id of the action.requested event this answers, and
	// DurableID the store's row for it, when the store holds approvals.
	RequestID string
	DurableID string
	Tool      string
	Args      json.RawMessage
	Reason    string
	Scope     string

	state    askState
	approved bool
	scope    string // the "always allow" scope sent with the answer
	approver string // who sent the answer, when they signed in
	// ready is closed once DurableID is settled, answer when an answer is
	// held, and final when the request is taken or ended.
	ready, answer, final chan struct{}
}

const (
	// requestWait is how long an answer naming a request waits for the turn
	// to publish it: the event reaches the client just before the turn waits.
	requestWait = 2 * time.Second
	// approvalReadyWait bounds how long an answer waits for its request's row.
	approvalReadyWait = 30 * time.Second
	// maxParked bounds the answers one session holds waiting at a time.
	maxParked = 8
	// endedKept is how many requests that stopped waiting a session remembers.
	endedKept = 64
	// rowEndWait bounds closing a request's row once it stops waiting.
	rowEndWait = 5 * time.Second
)

// move makes one transition, under l.mu, and reports whether it was made.
// Only waiting -> answered, waiting|answered -> taken and waiting|answered ->
// ended exist; anything else leaves the request as it is.
func (l *liveSession) move(p *pendingApproval, to askState, approved bool, scope string) bool {
	switch {
	case to == askAnswered && p.state == askWaiting:
		p.approved, p.scope = approved, scope
		close(p.answer)
	case (to == askTaken || to == askEnded) && (p.state == askWaiting || p.state == askAnswered):
		if to == askTaken && p.state == askWaiting {
			p.approved, p.scope = approved, scope
		}
		close(p.final)
	default:
		return false
	}
	p.state = to
	return true
}

// forget records a request that stopped waiting, oldest dropped first, so a
// late answer naming it is refused at once.
func (l *liveSession) forget(requestID string) {
	if requestID == "" {
		return
	}
	if l.ended == nil {
		l.ended = map[string]bool{}
	}
	if len(l.endedOrder) >= endedKept {
		delete(l.ended, l.endedOrder[0])
		l.endedOrder = l.endedOrder[1:]
	}
	l.ended[requestID] = true
	l.endedOrder = append(l.endedOrder, requestID)
}

// pendingFor returns the request an answer applies to, or why there is none.
// An answer naming a request applies only to that one.
func (l *liveSession) pendingFor(ctx context.Context, requestID string) (*pendingApproval, string) {
	deadline := time.Now().Add(requestWait)
	for {
		l.mu.Lock()
		p, ended := l.pending, l.ended[requestID]
		l.mu.Unlock()
		switch {
		case p != nil && (requestID == "" || p.RequestID == requestID):
			return p, ""
		case p != nil || ended:
			return nil, "that approval is no longer pending"
		case requestID == "" || time.Now().After(deadline):
			return nil, "no approval is pending for this session"
		}
		select {
		case <-ctx.Done():
			return nil, "no approval is pending for this session"
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// answerHere answers a request on the node running its session. The reply
// says what became of the answer: 204 the turn took it; 200 recorded on the
// store but the turn ended without taking it; 409 already answered, ended or
// not pending; 429 too many answers waiting, retry; 503 the row was not
// written in time.
func (s *Server) answerHere(w http.ResponseWriter, r *http.Request, live *liveSession, req approveRequest) {
	live.mu.Lock()
	if live.parked >= maxParked {
		live.mu.Unlock()
		// Busy, not refused: the request may still be waiting, so ask again.
		w.Header().Set("Retry-After", "1")
		WriteError(w, http.StatusTooManyRequests, "too many answers are waiting for this session")
		return
	}
	live.parked++
	live.mu.Unlock()
	defer func() { live.mu.Lock(); live.parked--; live.mu.Unlock() }()

	p, msg := live.pendingFor(r.Context(), req.RequestID)
	if p == nil {
		WriteError(w, http.StatusConflict, msg)
		return
	}
	// "Always allow" may only remember the scope the request offered, never
	// a wider one the client chose.
	if req.Scope != "" && req.Scope != p.Scope {
		WriteError(w, http.StatusBadRequest, "scope must be empty or the scope this request offered")
		return
	}
	// Published before its row is written, so the turn can be answered at
	// once; the answer waits for the row it must be recorded on.
	select {
	case <-p.ready:
	case <-p.final:
	case <-r.Context().Done():
		return
	case <-time.After(approvalReadyWait):
		WriteError(w, http.StatusServiceUnavailable, "the approval is still being recorded; retry")
		return
	}
	live.mu.Lock()
	state, durableID := p.state, p.DurableID
	live.mu.Unlock()
	if state != askWaiting {
		refuse(w, state)
		return
	}

	// The store's row is the one arbiter between answers: the first to
	// record it decides, and the turn reads it whichever node answered.
	recorded := false
	if d := s.approvalStore(); d != nil && durableID != "" {
		answered, err := d.AnswerApproval(r.Context(), durableID, req.Approved, req.Scope, UserOf(r.Context()))
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "could not record the decision")
			return
		}
		if !answered {
			// Either another answer won the row, or the request ended and closed it.
			live.mu.Lock()
			state = p.state
			live.mu.Unlock()
			refuse(w, state)
			return
		}
		recorded = true
	}

	live.mu.Lock()
	held := live.move(p, askAnswered, req.Approved, req.Scope)
	if held {
		p.approver = approverOf(r.Context())
	}
	state = p.state
	live.mu.Unlock()
	// Having won the row, this answer is the decision even if the turn read
	// it from the row first; without a row, only the one that was held is.
	if !held && (!recorded || state != askTaken) {
		if recorded && state == askEnded {
			writeRecordedNotApplied(w)
			return
		}
		refuse(w, state)
		return
	}
	select {
	case <-p.final:
	case <-r.Context().Done():
		return
	}
	live.mu.Lock()
	state = p.state
	live.mu.Unlock()
	switch {
	case state == askTaken:
		w.WriteHeader(http.StatusNoContent)
	case recorded:
		writeRecordedNotApplied(w)
	default:
		WriteError(w, http.StatusConflict, "that approval is no longer pending")
	}
}

func refuse(w http.ResponseWriter, state askState) {
	if state == askEnded {
		WriteError(w, http.StatusConflict, "that approval is no longer pending")
		return
	}
	WriteError(w, http.StatusConflict, "this approval was already answered")
}

// writeRecordedNotApplied says the decision is on the record but the turn
// ended without acting on it, so nothing ran on this answer.
func writeRecordedNotApplied(w http.ResponseWriter) {
	WriteJSON(w, http.StatusOK, map[string]bool{"recorded": true, "applied": false})
}

// Approve implements agent.Approver for a server session: it publishes the
// pending request and blocks until a reviewer answers or the session is
// cancelled. This is what enables headless runs with a human gate.
func (l *liveSession) Approve(ctx context.Context, tool string, args json.RawMessage, res policy.Result) (bool, error) {
	// Already allowed for this session: a reviewer chose "always allow" for
	// this scope earlier, so proceed without asking again. An ask rule or a
	// destructive command offers no scope, so it always asks.
	offer := res.Offer()
	if offer != "" {
		l.mu.Lock()
		remembered := l.allowed[offer]
		l.mu.Unlock()
		if remembered {
			agent.NoteAnswer(ctx, agent.Answer{By: agent.BySessionScope, Scope: offer})
			return true, nil
		}
	}

	p := &pendingApproval{RequestID: agent.RequestIDOf(ctx), Tool: tool, Args: args, Reason: res.Reason, Scope: offer,
		ready: make(chan struct{}), answer: make(chan struct{}), final: make(chan struct{})}
	l.mu.Lock()
	l.State = "waiting_approval"
	l.pending = p
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		l.move(p, askEnded, false, "") // a no-op once taken
		l.State = "running"
		if l.pending == p {
			l.pending = nil
		}
		l.forget(p.RequestID)
		durableID := p.DurableID
		l.mu.Unlock()
		// However the request ended, its row takes no later answer. Closed
		// off the turn's path, so a slow store does not delay its end.
		if durableID != "" {
			go l.endRow(ctx, durableID)
		}
	}()

	// Record it durably where the store can. The answer may arrive at
	// another node, and a channel in this process is not reachable from
	// there. A failure to record is not a reason to refuse the turn: the
	// in-memory path below still works for an answer that lands here.
	// The turn does not wait on a slow write once it is interrupted, so a
	// shutdown still records its end promptly.
	type row struct {
		id  string
		err error
	}
	written := make(chan row, 1)
	// The id is chosen here, so a row whose write timed out after it committed
	// can still be found and closed.
	rowID := newApprovalID()
	if l.durable != nil {
		go func() {
			// Not cut short by the interrupt: a write cancelled mid-flight may
			// still commit, and then nothing would know to close its row.
			wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), approvalReadyWait)
			defer cancel()
			id, err := l.durable.AskApproval(wctx, store.Approval{
				ID: rowID, SessionID: l.ID, Tool: tool, Args: args,
				Reason: res.Reason, Scope: offer,
			})
			if err != nil {
				go l.endRow(ctx, rowID)
			}
			written <- row{id, err}
		}()
	} else {
		written <- row{}
	}
	var durableID string
	select {
	case w := <-written:
		if w.err == nil {
			durableID = w.id
		}
	case <-ctx.Done():
		// The turn has given up; a row written after this is closed here.
		go func() {
			if w := <-written; w.err == nil && w.id != "" {
				l.endRow(ctx, w.id)
			}
		}()
	}
	l.mu.Lock()
	p.DurableID = durableID
	if ctx.Err() != nil {
		// Interrupted while the row was written: ended before any answer
		// waiting on ready can be held.
		l.move(p, askEnded, false, "")
	}
	l.mu.Unlock()
	close(p.ready)
	if err := ctx.Err(); err != nil {
		return false, err
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
			// An answer held but not yet taken loses to the interrupt, and
			// its sender is told so.
			return false, ctx.Err()

		case <-p.answer:
			// Interrupted as the answer arrived: the interrupt wins, and the
			// sender is told the request ended.
			if ctx.Err() != nil {
				agent.NoteAnswer(ctx, agent.Answer{Held: true})
				return false, ctx.Err()
			}
			l.mu.Lock()
			took := l.move(p, askTaken, false, "")
			approved, scope, approver := p.approved, p.scope, p.approver
			l.mu.Unlock()
			if !took {
				agent.NoteAnswer(ctx, agent.Answer{Held: true})
				return false, ctx.Err()
			}
			// "Always allow" carries the scope back; remember it so the next call
			// matching the same rule is not re-prompted.
			if approved && scope != "" && scope == offer {
				l.mu.Lock()
				l.allowed[scope] = true
				l.mu.Unlock()
			} else {
				scope = ""
			}
			agent.NoteAnswer(ctx, agent.Answer{By: agent.ByReviewer, Approver: approver, Granted: scope})
			return approved, nil

		case <-poll:
			approved, answered, scope, err := l.durable.ApprovalResult(ctx, durableID)
			if err != nil || !answered {
				continue
			}
			if ctx.Err() != nil {
				agent.NoteAnswer(ctx, agent.Answer{Held: true})
				return false, ctx.Err()
			}
			l.mu.Lock()
			took := l.move(p, askTaken, approved, scope)
			l.mu.Unlock()
			if !took {
				agent.NoteAnswer(ctx, agent.Answer{Held: true})
				return false, ctx.Err()
			}
			// Only an answer that chose "always allow" widens the session, and
			// only to the scope this request offered.
			if approved && scope != "" && scope == offer {
				l.mu.Lock()
				l.allowed[scope] = true
				l.mu.Unlock()
			} else {
				scope = ""
			}
			var approver string
			if a, ok := l.durable.(approvalAnswerer); ok {
				approver, _ = a.ApprovalAnsweredBy(ctx, durableID)
			}
			// The row says "anonymous" when no one signed in; the record names no one then.
			if approver == "anonymous" {
				approver = ""
			}
			agent.NoteAnswer(ctx, agent.Answer{By: agent.ByReviewer, Approver: approver, Granted: scope})
			return approved, nil

		case <-deadline.C:
			// Fail closed: an unanswered approval must not become an approval.
			agent.NoteAnswer(ctx, agent.Answer{By: agent.BySystem, Reason: "no answer within 30 minutes"})
			return false, nil
		}
	}
}

// approvalAnswerer is a store that can also say who answered a request.
type approvalAnswerer interface {
	ApprovalAnsweredBy(ctx context.Context, id string) (string, error)
}

// approverOf is the signed-in person sending an answer, or "" when there is
// none: the auth layer names an unsigned request "anonymous".
func approverOf(ctx context.Context) string {
	if id, _ := auth.FromContext(ctx); id == nil || UserOf(ctx) == "anonymous" {
		return ""
	}
	return UserOf(ctx)
}

// newApprovalID is an unguessable id for a request's row: the id is the
// capability to answer it.
func newApprovalID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("server: crypto/rand unavailable: " + err.Error())
	}
	return "ap-" + hex.EncodeToString(b[:])
}

// endRow closes a request's row, bounded by rowEndWait.
func (l *liveSession) endRow(ctx context.Context, id string) {
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rowEndWait)
	defer cancel()
	_ = l.durable.EndApproval(ectx, id)
}
