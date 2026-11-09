package app

import (
	"fmt"

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

// editorCharge charges an editor call to the session's budget. A call cut
// before usage was reported is estimated from the characters sent and
// received, about four to a token, as a cut turn is.
func editorCharge(l *agent.Loop, a model.Adapter, in, out int, cut bool, sentChars, gotChars int) (cost *float64, unpriced, estimated bool) {
	if cut && in == 0 && out == 0 {
		in, out = (sentChars+3)/4, (gotChars+3)/4
		estimated = in > 0 || out > 0
	}
	cost, unpriced = l.ChargeEditorCall(a.Profile().Name, model.Usage{InputTokens: in, OutputTokens: out})
	return cost, unpriced, estimated
}

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
