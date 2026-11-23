package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ui"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// rpcApproval is an ask put to the rpc caller, as its "approval" line carries it.
type rpcApproval struct {
	RequestID   string          `json:"request_id"`
	CallID      string          `json:"call_id,omitempty"`
	Tool        string          `json:"tool"`
	Args        json.RawMessage `json:"args"`
	Reason      string          `json:"reason,omitempty"`
	Step        string          `json:"step,omitempty"`
	Rule        string          `json:"rule,omitempty"`
	Source      string          `json:"source,omitempty"`
	Offer       string          `json:"offer,omitempty"`
	Destructive bool            `json:"destructive,omitempty"`
	ArgsHidden  bool            `json:"args_hidden,omitempty"`
	Subagent    string          `json:"subagent,omitempty"`
	TaskID      string          `json:"task_id,omitempty"`
	Via         string          `json:"via,omitempty"`
}

// rpcMaxApprover bounds the approver name an answer may record.
const rpcMaxApprover = 256

// rpcAnswer is the caller's answer to one approval.
type rpcAnswer struct {
	allow    bool
	remember bool
	approver string
}

// rpcApprovals holds the asks waiting for the caller, across sessions: an
// approve request is matched by its request id, and input ending refuses
// every ask still open, since no answer can arrive.
type rpcApprovals struct {
	mu      sync.Mutex
	waiting map[string]chan rpcAnswer
	ended   bool
	endedCh chan struct{}
	n       int
}

func newRPCApprovals() *rpcApprovals {
	return &rpcApprovals{waiting: map[string]chan rpcAnswer{}, endedCh: make(chan struct{})}
}

// open registers an ask and returns its id and channel; ok is false once input has ended.
func (p *rpcApprovals) open(requestID string) (string, chan rpcAnswer, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return "", nil, false
	}
	id := requestID
	if _, taken := p.waiting[id]; id == "" || taken {
		p.n++
		id = fmt.Sprintf("ask-%d", p.n)
	}
	ch := make(chan rpcAnswer, 1)
	p.waiting[id] = ch
	return id, ch, true
}

func (p *rpcApprovals) close(id string) {
	p.mu.Lock()
	delete(p.waiting, id)
	p.mu.Unlock()
}

// answer delivers an approve request's answer; false when no ask has that id.
func (p *rpcApprovals) answer(id string, a rpcAnswer) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, ok := p.waiting[id]
	if !ok {
		return false
	}
	delete(p.waiting, id)
	ch <- a
	return true
}

// end refuses every ask still open and every later one.
func (p *rpcApprovals) end() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ended {
		p.ended = true
		close(p.endedCh)
	}
}

// approve answers an approve request on the reader goroutine, so an ask is
// settled while the prompt that made it is running.
func (p *rpcApprovals) approve(req rpcRequest) rpcResponse {
	if req.RequestID == "" {
		return rpcResponse{ID: req.ID, Type: "error", Error: `approve needs the ask's "request_id"`}
	}
	var a rpcAnswer
	switch req.Decision {
	case "allow":
		a.allow = true
	case "deny":
	default:
		return rpcResponse{ID: req.ID, Type: "error", Error: `approve needs "decision": "allow" or "deny"`}
	}
	if len([]rune(req.Approver)) > rpcMaxApprover {
		return rpcResponse{ID: req.ID, Type: "error", Error: fmt.Sprintf(`"approver" is longer than %d characters`, rpcMaxApprover)}
	}
	if strings.ContainsFunc(req.Approver, func(r rune) bool { return !unicode.IsPrint(r) }) || ui.HasHidden(req.Approver) {
		return rpcResponse{ID: req.ID, Type: "error", Error: `"approver" holds a character that does not print as itself`}
	}
	a.remember, a.approver = req.Remember, req.Approver
	if !p.answer(req.RequestID, a) {
		return rpcResponse{ID: req.ID, Type: "error", Error: "no approval is waiting with request_id " + req.RequestID}
	}
	return rpcResponse{ID: req.ID, Type: "answered"}
}

// rpcApprover asks the caller, over approval lines, about each call that
// needs approval in one session. A scope the caller chose to remember
// answers later asks it covers without asking again.
func rpcApprover(p *rpcApprovals, emit func(any)) func(context.Context, string, json.RawMessage, abhed.Decision) (bool, error) {
	var mu sync.Mutex
	always := map[string]string{} // scope -> guard
	return func(ctx context.Context, tool string, args json.RawMessage, d abhed.Decision) (bool, error) {
		offer := d.Offer()
		mu.Lock()
		guard, ok := always[offer]
		mu.Unlock()
		if ok && d.Covers(offer, guard) {
			abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.BySessionScope, Scope: offer})
			return true, nil
		}
		ask := rpcApproval{CallID: abhed.CallIDOf(ctx), Tool: tool, Args: args, Reason: d.Reason,
			Step: d.Step, Rule: ruleOf(d), Source: d.Source, Offer: offer,
			Destructive: destructive(tool, args, d), Subagent: agent.SubagentOf(ctx),
			TaskID: agent.BackgroundTaskOf(ctx)}
		// The caller is shown the request as recorded; one the record
		// withheld cannot be reviewed, so it is refused without asking.
		if rec, ok := agent.RequestedOf(ctx); ok {
			if rec.Withheld {
				abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.BySystem, Reason: "the request was withheld from the record, so it cannot be shown for review"})
				return false, nil
			}
			ask.Args, ask.Reason, ask.Via = rec.Args, rec.Reason, rec.Via
			if offer != "" {
				ask.Offer = rec.Scope
			}
		}
		ask.ArgsHidden = ui.ArgsHidden(args) || ui.ArgsHidden(ask.Args) || ui.HasHidden(tool)
		id, ch, ok := p.open(abhed.RequestIDOf(ctx))
		if !ok {
			abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.BySystem, Reason: "rpc input ended, so no answer can arrive"})
			return false, nil
		}
		defer p.close(id)
		ask.RequestID = id
		emit(rpcResponse{Type: "approval", Approval: &ask})
		select {
		case a := <-ch:
			// An answer that arrives as the run is stopped loses to the stop:
			// it is never applied, and the record says the request was interrupted.
			if ctx.Err() != nil {
				agent.NoteAnswer(ctx, agent.Answer{Held: true})
				return false, ctx.Err()
			}
			if !a.allow {
				if a.approver != "" {
					abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.ByReviewer, Approver: a.approver})
				}
				return false, nil
			}
			ans := abhed.Answer{By: abhed.ByReviewer, Approver: a.approver}
			// Only the scope policy offered may be remembered.
			if a.remember && offer != "" {
				mu.Lock()
				always[offer] = d.Guard
				mu.Unlock()
				ans.Granted = offer
			}
			abhed.NoteAnswer(ctx, ans)
			return true, nil
		case <-p.endedCh:
			abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.BySystem, Reason: "rpc input ended before the approval was answered"})
			return false, nil
		case <-ctx.Done():
			// Recorded as interrupted, as any stop while an approval is awaited.
			return false, ctx.Err()
		}
	}
}
