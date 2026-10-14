package controlplane

import (
	"context"
	"slices"
	"time"
)

// Outcomes an audit event records.
const (
	OutcomeRequested = "requested"
	OutcomeOK        = "ok"
	OutcomeFailed    = "failed"
)

// SystemPrincipal is who the reconciler acts as in the record.
const SystemPrincipal = "system:reconciler"

// AuditEvent is one mutation of a sandbox as the record keeps it. Every
// mutating call writes one before it acts and one with its outcome.
type AuditEvent struct {
	Seq        int64     `json:"seq,omitempty"`
	At         time.Time `json:"at"`
	Principal  string    `json:"principal"`
	Action     string    `json:"action"`
	SandboxID  string    `json:"sandbox_id"`
	Generation int64     `json:"generation"`
	Outcome    string    `json:"outcome"`
	// Code is the error code of a failed outcome.
	Code string `json:"code,omitempty"`
	// Detail is what the call asked for, redacted; never a secret.
	Detail map[string]string `json:"detail,omitempty"`
}

// AuditLog is the append-only record of control-plane mutations.
type AuditLog interface {
	Append(ctx context.Context, ev AuditEvent) error
}

// AuditReader lists the record for one sandbox, oldest first.
type AuditReader interface {
	AuditFor(ctx context.Context, sandboxID string) ([]AuditEvent, error)
}

// Append adds ev to the memory store's record.
func (m *Memory) Append(_ context.Context, ev AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditSeq++
	ev.Seq = m.auditSeq
	m.audit = append(m.audit, ev)
	return nil
}

// AuditFor lists the record of one sandbox.
func (m *Memory) AuditFor(_ context.Context, id string) ([]AuditEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []AuditEvent
	for _, ev := range m.audit {
		if ev.SandboxID == id {
			out = append(out, ev)
		}
	}
	return slices.Clone(out), nil
}
