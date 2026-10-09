package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Operations an Interceptor is asked about: every call that changes a
// sandbox or runs something in it.
const (
	OpCreate = "create"
	OpUpdate = "update"
	OpStart  = "start"
	OpStop   = "stop"
	OpDelete = "delete"
	OpExec   = "exec"
)

// Operations lists every operation, in the order the contract names them.
var Operations = []string{OpCreate, OpUpdate, OpStart, OpStop, OpDelete, OpExec}

// ErrDenied is a call an admission interceptor refused (403, code forbidden).
var ErrDenied = errors.New("denied by admission")

// AdmissionRequest is one call put to an Interceptor before it is committed.
type AdmissionRequest struct {
	Operation string
	Caller    Caller
	SandboxID string
	// Old is the sandbox before the call; nil on create.
	Old *Sandbox
	// Object is the sandbox as the call would leave it.
	Object Sandbox
	// Command is an exec's command line, already redacted for the record.
	Command string
}

// AdmissionDecision is what one hook decided. Each is recorded with the
// caller's principal before the call goes on.
type AdmissionDecision struct {
	Hook string
	// Phase is validate, patch or post_commit.
	Phase string
	// Decision is allow, deny, patch or error.
	Decision string
	Reason   string
	// Patched names the fields a patch changed, as JSON pointers.
	Patched []string
	// FailOpen marks an error the hook is configured to let through.
	FailOpen bool
}

// AdmissionResult is an Interceptor's answer: the object to commit and
// every decision made on the way to it.
type AdmissionResult struct {
	Object    Sandbox
	Decisions []AdmissionDecision
}

// Interceptor admits every mutating call before it is committed and hears
// how it ended. The Enterprise module implements it; nil admits everything.
//
// Admit returns the object to commit. It may change only the object's
// labels outside the reserved prefix and its settings, and settings only
// tighter; the service checks the result again and refuses anything else.
// An error refuses the call: ErrDenied for a refusal, ErrUnavailable for a
// hook that could not answer. Decisions are recorded even with an error.
type Interceptor interface {
	Admit(ctx context.Context, req AdmissionRequest) (AdmissionResult, error)
	// Committed runs after the call, with how it ended. It cannot undo it.
	Committed(ctx context.Context, req AdmissionRequest, out Sandbox, err error) []AdmissionDecision
}

// admit puts req to the interceptor, records each decision, and returns the
// object to commit. Anything the interceptor changed beyond what it may is
// refused, as is a decision the record cannot keep.
func (s *Service) admit(ctx context.Context, req AdmissionRequest) (Sandbox, error) {
	if s.cfg.Interceptor == nil {
		return req.Object, nil
	}
	in := req.Object.Clone()
	req.Object = req.Object.Clone()
	res, err := s.cfg.Interceptor.Admit(ctx, req)
	if rerr := s.recordDecisions(ctx, req, res.Decisions); rerr != nil {
		return Sandbox{}, rerr
	}
	if err != nil {
		if errors.Is(err, ErrDenied) || errors.Is(err, ErrUnavailable) {
			return Sandbox{}, err
		}
		// An interceptor that fails in a way it did not name still refuses.
		return Sandbox{}, fmt.Errorf("%w: admission failed: %w", ErrUnavailable, err)
	}
	if err := s.checkAdmitted(req.Operation, in, res.Object); err != nil {
		_ = s.recordDecisions(ctx, req, []AdmissionDecision{{Hook: "abhed", Phase: "patch", Decision: "deny", Reason: err.Error()}})
		return Sandbox{}, fmt.Errorf("%w: %w", ErrDenied, err)
	}
	return res.Object.Clone(), nil
}

// committed tells the interceptor how a call ended and records what it says.
func (s *Service) committed(ctx context.Context, req AdmissionRequest, out Sandbox, err error) {
	if s.cfg.Interceptor == nil {
		return
	}
	ds := s.cfg.Interceptor.Committed(context.WithoutCancel(ctx), req, out.Clone(), err)
	_ = s.recordDecisions(ctx, req, ds)
}

// maxReason bounds the text a hook may put in the record.
const maxReason = 512

func (s *Service) recordDecisions(ctx context.Context, req AdmissionRequest, ds []AdmissionDecision) error {
	for _, d := range ds {
		ev := AuditEvent{Principal: req.Caller.Principal, Action: "admission", SandboxID: req.SandboxID,
			Generation: req.Object.Generation, Outcome: OutcomeOK,
			Detail: map[string]string{"operation": req.Operation, "hook": d.Hook, "phase": d.Phase, "decision": d.Decision}}
		if d.Decision == "deny" || d.Decision == "error" {
			ev.Outcome, ev.Code = OutcomeFailed, "forbidden"
			if d.Decision == "error" {
				ev.Code = "unavailable"
			}
		}
		if d.Reason != "" {
			r := d.Reason
			if len(r) > maxReason {
				r = r[:maxReason]
			}
			// Hook text is untrusted and may echo what it was sent.
			ev.Detail["reason"] = s.redact(r)
		}
		if len(d.Patched) > 0 {
			ev.Detail["patched"] = strings.Join(d.Patched, ",")
		}
		if d.FailOpen {
			ev.Detail["fail_open"] = "true"
		}
		if err := s.record(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// checkAdmitted refuses an admitted object that differs from the proposed
// one in anything but unreserved labels and tighter settings. A template
// change is refused too: a template may carry a looser policy.
func (s *Service) checkAdmitted(op string, in, out Sandbox) error {
	a, b := in.Clone(), out.Clone()
	a.Labels, b.Labels, a.Settings, b.Settings = nil, nil, nil, nil
	// Compared as JSON, the form the object is stored and sent to hooks in.
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	if errA != nil || errB != nil || !bytes.Equal(ja, jb) {
		return fmt.Errorf("%w: an interceptor may change only labels and settings", ErrInvalid)
	}
	if op == OpExec && (!maps.Equal(in.Labels, out.Labels) || !maps.Equal(in.Settings, out.Settings)) {
		return fmt.Errorf("%w: an exec cannot be patched", ErrInvalid)
	}
	for k, v := range in.Labels {
		if strings.HasPrefix(k, ReservedPrefix) && out.Labels[k] != v {
			return fmt.Errorf("%w: label %q is reserved", ErrInvalid, k)
		}
	}
	for k := range out.Labels {
		if _, had := in.Labels[k]; !had && strings.HasPrefix(k, ReservedPrefix) {
			return fmt.Errorf("%w: label %q is reserved", ErrInvalid, k)
		}
	}
	if err := ValidateLabels(out.Labels, true); err != nil {
		return err
	}
	if err := s.cfg.Settings.Check(out.Settings); err != nil {
		return err
	}
	return s.cfg.Settings.NoLooser(in.Settings, out.Settings)
}

// NoLooser refuses after when any setting it puts in force for a sandbox
// is looser than what before put in force.
func (st *Settings) NoLooser(before, after map[string]string) error {
	was, now := st.For(before), st.For(after)
	keys := slices.Sorted(maps.Keys(now))
	for _, k := range keys {
		def, ok := lookup(k)
		if !ok || def.Scope != ScopeSandbox {
			continue
		}
		rb, errB := def.rank(was[k])
		ra, errA := def.rank(now[k])
		if errA != nil || (errB == nil && ra < rb) {
			return fmt.Errorf("%w: %s=%q is looser than %q", ErrInvalid, k, now[k], was[k])
		}
	}
	return nil
}
