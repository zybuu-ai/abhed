package controlplane

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
)

// Guard admits or refuses an operation a caller asks of the service, after
// the request is recorded and before the store or a driver is touched. The
// Team edition's workspaces, roles, quotas and four-eyes approvals are a
// Guard; with none installed every operation is admitted as before.
//
// Admit must fail closed: when it cannot decide, such as when usage cannot be
// counted, it returns an error and the operation does not happen. The release
// it returns, when not nil, runs once the operation has ended, whatever its
// outcome, so a guard can hold a slot or a lock across it.
type Guard interface {
	Admit(ctx context.Context, a *Admission) (release func(), err error)
}

// Actions a Guard is asked to admit.
const (
	ActionCreate = "create"
	ActionUpdate = "update"
	ActionStart  = "start"
	ActionStop   = "stop"
	ActionDelete = "delete"
	ActionExec   = "exec"
)

// Admission is one operation as a Guard sees it.
type Admission struct {
	Action string
	Caller Caller
	// Sandbox is the object as it is now; zero for a create.
	Sandbox Sandbox
	// Next is the object as the operation would leave it, for a create and an
	// update. On a create a guard may tighten Next.Settings; the result is
	// checked against the settings registry again before it is stored.
	Next *Sandbox
	// Exec is the command of an exec.
	Exec *ExecRequest
	// Grace is a stop's grace period.
	Grace time.Duration
	// Approval is the approval the request names, "" when it names none.
	Approval string
}

// SetGuard installs g. It is set once, before the service serves; a second
// guard is refused so nothing can quietly replace the first.
func (s *Service) SetGuard(g Guard) error {
	if g == nil {
		return errors.New("control plane: a nil guard")
	}
	if !s.guard.CompareAndSwap(nil, &guardBox{g}) {
		return errors.New("control plane: a guard is already installed")
	}
	return nil
}

type guardBox struct{ Guard }

// guardAdmit asks the installed guard, if any, about a.
func (s *Service) guardAdmit(ctx context.Context, a *Admission) (func(), error) {
	box := s.guard.Load()
	if box == nil {
		return func() {}, nil
	}
	a.Approval = ApprovalOf(ctx)
	release, err := box.Admit(ctx, a)
	if err != nil {
		if release != nil {
			release()
		}
		return nil, guardErr(err)
	}
	if release == nil {
		release = func() {}
	}
	return release, nil
}

// guardErr keeps a guard's refusal as it is, and turns anything else into
// ErrUnavailable, so a guard that fails never reads as an admission.
func guardErr(err error) error {
	for _, known := range []error{ErrForbidden, ErrApprovalRequired, ErrQuotaExceeded, ErrInvalid, ErrNotFound,
		ErrUnavailable, ErrInvalidTransition} {
		if errors.Is(err, known) {
			return err
		}
	}
	return fmt.Errorf("%w: the guard could not decide: %w", ErrUnavailable, err)
}

type approvalKey struct{}

// WithApproval is ctx carrying the approval a request names, which the
// guard checks. The REST surface reads it from the Abhed-Approval header.
func WithApproval(ctx context.Context, id string) context.Context {
	id = strings.TrimSpace(id)
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, approvalKey{}, id)
}

// ApprovalOf is the approval ctx carries, "" for none.
func ApprovalOf(ctx context.Context) string {
	id, _ := ctx.Value(approvalKey{}).(string)
	return id
}

// ApprovalNeeded refuses an operation until other people approve it. ID
// names the approval to ask them for, and the request is sent again naming
// it once they have.
type ApprovalNeeded struct {
	ID        string
	ExpiresAt time.Time
	Message   string
}

func (e *ApprovalNeeded) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "this needs approval by other people first"
	}
	return fmt.Sprintf("%s: %s (approval %s)", ErrApprovalRequired, msg, e.ID)
}

func (e *ApprovalNeeded) Unwrap() error { return ErrApprovalRequired }

// scoped reports whether labels carry every label of scope. An empty value
// in scope matches nothing, so a scope can never match unlabelled sandboxes.
func scoped(scope, labels map[string]string) bool {
	for k, v := range scope {
		if v == "" || labels[k] != v {
			return false
		}
	}
	return true
}

// checkScope refuses a scope that is not made of reserved labels: anything
// else a client could set on a sandbox and so reach into the scope.
func checkScope(scope map[string]string) error {
	for k, v := range scope {
		if !strings.HasPrefix(k, ReservedPrefix) || v == "" {
			return fmt.Errorf("%w: caller scope %s=%q is not a reserved label with a value", ErrForbidden, k, v)
		}
	}
	return ValidateLabels(maps.Clone(scope), true)
}
