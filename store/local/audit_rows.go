package local

import (
	"context"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/store/chain"
)

// AuditChainName names the local audit file's chain.
const AuditChainName = "local/audit"

// AuditRows gives the audit file's lines at path in order, each with its
// place in the chain: its line number from 1, the hash before it and its own.
func AuditRows(_ context.Context, path string, fn func(controlplane.AuditEvent, chain.Link) error) error {
	lines, err := readAudit(path)
	if err != nil {
		return err
	}
	for i, l := range lines {
		at, _ := time.Parse(time.RFC3339Nano, l.At)
		ev := controlplane.AuditEvent{Seq: l.Seq, At: at, Principal: l.Principal, Action: l.Action, SandboxID: l.SandboxID,
			Generation: l.Generation, Outcome: l.Outcome, Code: l.Code, Detail: l.Detail}
		if err := fn(ev, chain.Link{Pos: int64(i + 1), Prev: l.Prev, Hash: l.Hash}); err != nil {
			return err
		}
	}
	return nil
}
