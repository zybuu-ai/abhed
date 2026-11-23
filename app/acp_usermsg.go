package app

import (
	"encoding/json"
	"sync"

	"github.com/zybuu-ai/abhed/internal/agent"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// The editor pairs the message it drew with the user.message the engine
// records for it by seq. A prompt's is announced as soon as it is recorded
// (_abhed/session/userMessage, with the prompt's request id) and repeated
// in the prompt's reply; a steer's reply waits for its message to be
// recorded or dropped. A message refused or dropped before it is recorded
// gets userMessageSeq null and the reason.

// msgPairing is one session's messages awaiting their seq.
type msgPairing struct {
	mu sync.Mutex
	// prompt is the session/prompt request whose message is awaited; nil
	// when none is, and once its message is recorded or dropped.
	prompt json.RawMessage
	got    *msgOutcome
	// steers wait, by queue id, for a steered message's outcome; early holds
	// one that came before its steer began waiting.
	steers map[string]chan msgOutcome
	early  map[string]msgOutcome
}

// msgOutcome is a message's seq, or why it was not recorded.
type msgOutcome struct {
	seq    *int64
	reason string
}

func (o msgOutcome) meta(sessionID string) map[string]any {
	m := map[string]any{"sessionId": sessionID, "userMessageSeq": nil}
	if o.seq != nil {
		m["userMessageSeq"] = *o.seq
	} else {
		m["userMessageReason"] = o.reason
	}
	return m
}

// beginPrompt awaits the message of the prompt with request id.
func (p *msgPairing) beginPrompt(id json.RawMessage) {
	p.mu.Lock()
	p.prompt, p.got = id, nil
	p.mu.Unlock()
}

// endPrompt is the prompt's outcome; why stands when nothing was recorded.
func (p *msgPairing) endPrompt(why string) msgOutcome {
	p.mu.Lock()
	defer p.mu.Unlock()
	got := p.got
	p.prompt, p.got = nil, nil
	if got == nil {
		return msgOutcome{reason: why}
	}
	return *got
}

// awaitSteer returns where a steered message's outcome will arrive.
func (p *msgPairing) awaitSteer(queueID string) chan msgOutcome {
	ch := make(chan msgOutcome, 1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if out, ok := p.early[queueID]; ok {
		delete(p.early, queueID)
		ch <- out
		return ch
	}
	if p.steers == nil {
		p.steers = map[string]chan msgOutcome{}
	}
	p.steers[queueID] = ch
	return ch
}

// settleSteer answers the steer waiting on queueID, or keeps the answer for
// it when it has not begun waiting yet.
func (p *msgPairing) settleSteer(queueID string, out msgOutcome) {
	p.mu.Lock()
	ch := p.steers[queueID]
	delete(p.steers, queueID)
	if ch == nil {
		if p.early == nil || len(p.early) > 64 {
			// Bounded: a message queued from elsewhere has no steer waiting for it.
			p.early = map[string]msgOutcome{}
		}
		p.early[queueID] = out
	}
	p.mu.Unlock()
	if ch != nil {
		ch <- out
	}
}

// settleAll answers every steer still waiting, when the session closes.
func (p *msgPairing) settleAll(reason string) {
	p.mu.Lock()
	waiting := p.steers
	p.steers = nil
	p.mu.Unlock()
	for _, ch := range waiting {
		ch <- msgOutcome{reason: reason}
	}
}

func (p *msgPairing) forgetSteer(queueID string) {
	p.mu.Lock()
	delete(p.steers, queueID)
	p.mu.Unlock()
}

// noteUserMessage settles what awaits a recorded or dropped message, and
// announces a prompt's as soon as it is recorded.
func (c *acpConn) noteUserMessage(s *acpSession, ev abhed.Event) {
	var queueID, reason string
	var seq *int64
	switch ev.Type {
	case agent.EvUserMessage:
		var m agent.Message
		if json.Unmarshal(ev.Payload, &m) != nil {
			return
		}
		queueID, seq = m.QueueID, &ev.Seq
	case agent.EvMessageDropped:
		var d agent.DroppedMessage
		if json.Unmarshal(ev.Payload, &d) != nil {
			return
		}
		queueID, reason = d.QueueID, d.Reason
	default:
		return
	}
	out := msgOutcome{seq: seq, reason: reason}
	p := &s.pairing
	if queueID != "" {
		p.settleSteer(queueID, out)
		return
	}
	p.mu.Lock()
	prompt := p.prompt
	if prompt == nil || p.got != nil {
		p.mu.Unlock()
		return
	}
	p.got = &out
	p.mu.Unlock()
	c.notification("_abhed/session/userMessage", map[string]any{"sessionId": s.id, "requestId": prompt,
		"_meta": map[string]any{acpMetaKey: out.meta(s.id)}})
}
