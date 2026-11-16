package app

import (
	"errors"
	"fmt"
	"net"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
)

// editorRefused is the session budget's gate for an editor feature's call,
// asked before the request: "" lets it go. off names the feature, what its
// model. A local address is not taken as free: a proxy there may be paid.
func editorRefused(l *agent.Loop, a model.Adapter, off, what string) string {
	name := a.Profile().Name
	if l.Budget.USDLimited() {
		if _, ok := l.Budget.Price(name); !ok {
			return fmt.Sprintf("%s are off: the %s model has no price under this dollar cap; set pricing for it (0 for a local model)", off, what)
		}
	}
	if err := l.Budget.Refuse(name); err != nil {
		return off + " are off: " + err.Error()
	}
	return ""
}

// editorCharge charges an editor call to the session's budget, by the rules
// a turn's call is charged by (agent.EstimateUsage), counting from the
// characters sent and received at about four to a token. Only a call the
// provider never accepted (an error status, a connection never made) is not
// charged; any other failure came after it took the request. It returns the
// tokens charged with the cost.
func editorCharge(l *agent.Loop, a model.Adapter, in, out int, err error, cancelled bool, sentChars, gotChars, maxTokens int) (tokIn, tokOut int, cost *float64, unpriced, estimated bool) {
	if neverAccepted(err) {
		return 0, 0, nil, false, false
	}
	u, estimated := agent.EstimateUsage(charsAdapter{a, sentChars}, model.Request{MaxTokens: maxTokens},
		model.Usage{InputTokens: in, OutputTokens: out}, agent.CallEnd{Produced: gotChars, Cancelled: cancelled, Failed: err != nil},
		l.Budget.CutAtBound(a))
	cost, unpriced = l.ChargeEditorCall(a.Profile().Name, u)
	return u.InputTokens, u.OutputTokens, cost, unpriced, estimated
}

// neverAccepted reports an error from before the provider took the request:
// an error status, or a connection that was never made.
func neverAccepted(err error) bool {
	var se *model.StatusError
	var op *net.OpError
	var dns *net.DNSError
	return errors.As(err, &se) || errors.As(err, &dns) || errors.As(err, &op) && op.Op == "dial"
}

// charsAdapter counts an editor request by the characters it sent, which is
// all an editor call knows of its prompt once it has gone.
type charsAdapter struct {
	model.Adapter
	sent int
}

func (c charsAdapter) CountTokens(model.Request) (int, error) { return (c.sent + 3) / 4, nil }

// priceNotice is the field a refusal by the budget's gate is sent under: the
// first time per feature as "notice" too, so Studio shows it once.
func (k *acpCompleter) priceNotice(reply map[string]any, why string) map[string]any {
	k.mu.Lock()
	first := !k.noticed
	k.noticed = true
	k.sum.Refused++
	k.mu.Unlock()
	reply["disabled"] = why
	if first {
		reply["notice"] = why
	}
	return reply
}

// addCost adds a call's cost to a feature's summary.
func addCost(sum *agent.CompletionSummary, usd *float64, unpriced bool) {
	if usd != nil {
		sum.CostUSD += *usd
	}
	sum.Unpriced = sum.Unpriced || unpriced
}
