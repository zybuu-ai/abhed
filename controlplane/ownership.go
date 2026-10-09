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

// ErrOwnershipLost ends a long-lived call, such as a stream, on a sandbox
// this replica stopped owning while it ran. It is wrapped in ErrUnavailable,
// and the call's record names it as its reason.
var ErrOwnershipLost = errors.New("ownership lost")

// OwnershipWatcher is an Ownership that can tell when this replica stops
// owning a sandbox, so its open streams end rather than run on beside the
// new owner. An Ownership that does not implement it changes nothing.
type OwnershipWatcher interface {
	// Watch returns a channel closed once this replica no longer owns id;
	// nil when it cannot lose it. Watching stops when ctx ends.
	Watch(ctx context.Context, id string) <-chan struct{}
}

// OwnershipLost is closed once this replica stops owning id, for as long as
// ctx lasts; nil when nothing can tell, which never fires.
func (s *Service) OwnershipLost(ctx context.Context, id string) <-chan struct{} {
	w, ok := s.cfg.Ownership.(OwnershipWatcher)
	if !ok {
		return nil
	}
	return w.Watch(ctx, id)
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
