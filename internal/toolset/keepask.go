package toolset

import (
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// KeepManagedAsks is the policy engine's KeepAsk for cfg: an ask decided by
// an ask rule the managed configuration lists is not answered by a runbook.
func KeepManagedAsks(cfg config.Config) func(policy.Result) bool {
	return func(r policy.Result) bool { return r.Step == "ask" && cfg.ManagedAskRule(r.Rule) }
}
