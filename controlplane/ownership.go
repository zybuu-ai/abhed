package controlplane

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotOwner is a call for a sandbox another replica owns. It is always
// wrapped in ErrUnavailable, so it answers 503 and changes nothing.
var ErrNotOwner = errors.New("another replica owns this sandbox")

// Ownership says which sandboxes this replica acts on when several replicas
// share one store (Enterprise HA). Nil means this process owns them all.
type Ownership interface {
	// Owns reports whether this replica owns id, taking ownership when no
	// live replica holds it. An error means it cannot tell, and nothing acts.
	Owns(ctx context.Context, id string) (bool, error)
}

// owned refuses a call for a sandbox this replica does not own.
func (s *Service) owned(ctx context.Context, id string) error {
	if s.cfg.Ownership == nil {
		return nil
	}
	ok, err := s.cfg.Ownership.Owns(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: ownership of sandbox %s cannot be read: %w", ErrUnavailable, id, err)
	}
	if !ok {
		return fmt.Errorf("%w: %w: %s", ErrUnavailable, ErrNotOwner, id)
	}
	return nil
}
