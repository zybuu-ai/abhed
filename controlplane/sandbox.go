// Package controlplane holds Abhed's sandbox objects: their fields, their
// lifecycle, labels and selectors, the store they persist in, and the
// idempotency and resource-version rules every mutation follows.
//
// It is public, not internal, because the Team and Enterprise module plugs
// into it (interceptors see a Sandbox, HA wraps a Store) and can use only
// public packages. It is an experimental API until 2.0.
// docs/architecture/control-plane-contract.md is the contract.
package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// Sandbox is one isolated execution environment as the control plane
// records it.
type Sandbox struct {
	ID string `json:"id"`
	// Generation counts incarnations: it starts at 1 and goes up each time
	// the instance is started again, restored, or its template changes.
	Generation int64 `json:"generation"`
	// ResourceVersion goes up on every write; the store assigns it.
	ResourceVersion int64             `json:"resource_version"`
	Template        string            `json:"template"`
	PolicyRev       string            `json:"policy_rev,omitempty"`
	Driver          string            `json:"driver"`
	State           State             `json:"state"`
	Conditions      []Condition       `json:"conditions,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	// Owner is the principal that created it; clients never set it.
	Owner string `json:"owner"`
	// Tier is the isolation the driver reports it provides.
	Tier string `json:"tier,omitempty"`
	// Settings are this sandbox's values from the settings registry; they
	// may tighten what the server allows, never loosen a managed value.
	Settings map[string]string `json:"settings,omitempty"`
	// Restarts counts the restarts the server made since a client last
	// started the sandbox or it last ran stably.
	Restarts int `json:"restarts,omitempty"`
	// NextRestart, when set, is when the restart policy starts it again.
	NextRestart *time.Time `json:"next_restart,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Clone returns a copy sharing no maps or slices with s.
func (s Sandbox) Clone() Sandbox {
	s.Conditions = slices.Clone(s.Conditions)
	s.Labels = maps.Clone(s.Labels)
	s.Settings = maps.Clone(s.Settings)
	if s.NextRestart != nil {
		t := *s.NextRestart
		s.NextRestart = &t
	}
	return s
}

// ConditionStatus is True, False or Unknown.
type ConditionStatus string

const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// Condition types the server sets. Drivers add their own as "<driver>.<Type>".
const (
	CondDriverReady       = "DriverReady"
	CondPolicyApplied     = "PolicyApplied"
	CondIsolationVerified = "IsolationVerified"
	CondDeleteFailed      = "DeleteFailed"
	// CondRestartScheduled is True while a restart waits out its backoff,
	// and False once the restart budget is spent.
	CondRestartScheduled = "RestartScheduled"
)

// Condition is one observation about a sandbox.
type Condition struct {
	Type               string          `json:"type"`
	Status             ConditionStatus `json:"status"`
	Reason             string          `json:"reason,omitempty"`
	Message            string          `json:"message,omitempty"`
	ObservedGeneration int64           `json:"observed_generation"`
	LastTransition     time.Time       `json:"last_transition"`
}

// Condition returns the condition of type t, if s has one.
func (s *Sandbox) Condition(t string) (Condition, bool) {
	for _, c := range s.Conditions {
		if c.Type == t {
			return c, true
		}
	}
	return Condition{}, false
}

// SetCondition records c, replacing any of its type. LastTransition moves
// only when the status changes, so it says how long a condition has held.
func (s *Sandbox) SetCondition(c Condition, now time.Time) error {
	if c.Type == "" {
		return fmt.Errorf("%w: condition without a type", ErrInvalid)
	}
	switch c.Status {
	case ConditionTrue, ConditionFalse, ConditionUnknown:
	default:
		return fmt.Errorf("%w: condition status %q", ErrInvalid, c.Status)
	}
	if c.ObservedGeneration == 0 {
		c.ObservedGeneration = s.Generation
	}
	for i, old := range s.Conditions {
		if old.Type != c.Type {
			continue
		}
		c.LastTransition = old.LastTransition
		if old.Status != c.Status {
			c.LastTransition = now
		}
		s.Conditions[i] = c
		return nil
	}
	c.LastTransition = now
	s.Conditions = append(s.Conditions, c)
	return nil
}

// NewSandbox returns a sandbox in Provisioning, at generation 1, with a new id.
// Labels under the reserved prefix are refused; the server adds its own after.
func NewSandbox(owner, template, driverName string, labels map[string]string, now time.Time) (Sandbox, error) {
	if !driver.ValidName(driverName) {
		return Sandbox{}, fmt.Errorf("%w: driver name %q", ErrInvalid, driverName)
	}
	if err := validTemplate(template); err != nil {
		return Sandbox{}, err
	}
	if owner == "" {
		return Sandbox{}, fmt.Errorf("%w: a sandbox needs an owner", ErrInvalid)
	}
	if err := ValidateLabels(labels, false); err != nil {
		return Sandbox{}, err
	}
	id, err := NewID()
	if err != nil {
		return Sandbox{}, err
	}
	return Sandbox{
		ID: id, Generation: 1, Template: template, Driver: driverName, State: StateProvisioning,
		Labels: maps.Clone(labels), Owner: owner, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// SetTemplate changes the template, which takes effect at the next start,
// and so bumps the generation.
func (s *Sandbox) SetTemplate(t string) error {
	if err := validTemplate(t); err != nil {
		return err
	}
	if t != s.Template {
		s.Template = t
		s.Generation++
	}
	return nil
}

// validTemplate refuses an empty template name, or one that is long or
// holds anything but printable ASCII.
func validTemplate(t string) error {
	if t == "" {
		return fmt.Errorf("%w: a sandbox needs a template", ErrInvalid)
	}
	if len(t) > 253 {
		return fmt.Errorf("%w: a template name is at most 253 characters", ErrInvalid)
	}
	for i := 0; i < len(t); i++ {
		if t[i] < 0x21 || t[i] > 0x7e {
			return fmt.Errorf("%w: template name %q", ErrInvalid, t)
		}
	}
	return nil
}

const idPrefix = "sbx_"

// NewID returns a fresh sandbox id: sbx_ and 16 hex characters.
func NewID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return idPrefix + hex.EncodeToString(b[:]), nil
}

// ValidID reports whether id has the form NewID gives.
func ValidID(id string) bool {
	if len(id) != len(idPrefix)+16 || id[:len(idPrefix)] != idPrefix {
		return false
	}
	for _, c := range id[len(idPrefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Errors. A server maps them to HTTP statuses as the contract says.
var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	ErrInvalid  = errors.New("invalid")
	// ErrInvalidTransition is a verb the sandbox's state does not allow (409).
	ErrInvalidTransition = errors.New("invalid state transition")
	// ErrVersionMismatch is a write against a resource version that is no
	// longer current (412 with If-Match; retried otherwise).
	ErrVersionMismatch = errors.New("resource version mismatch")
	// ErrIdempotencyInFlight is a repeat of a request still running (409).
	ErrIdempotencyInFlight = errors.New("a request with this idempotency key is in progress")
	// ErrIdempotencyMismatch is a key reused with a different body (422).
	ErrIdempotencyMismatch = errors.New("idempotency key reused with a different request")
	// ErrUnavailable is a store, record or driver that cannot be reached
	// (503); nothing was changed.
	ErrUnavailable = errors.New("unavailable")
	// ErrForbidden is a caller that may not do this (403).
	ErrForbidden = errors.New("forbidden")
	// ErrApprovalRequired is an operation that waits on other people's
	// approval (403); see ApprovalNeeded.
	ErrApprovalRequired = errors.New("approval required")
	// ErrQuotaExceeded is an operation past a quota (403).
	ErrQuotaExceeded = errors.New("quota exceeded")
)
