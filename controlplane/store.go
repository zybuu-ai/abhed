package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Store persists sandboxes and idempotency records for one tenant. Memory,
// SQLite and Postgres implement it, and each must pass storetest.Run.
type Store interface {
	// Create stores s, which must not exist, and returns it with its
	// resource version.
	Create(ctx context.Context, s Sandbox) (Sandbox, error)
	// Get returns the sandbox, or ErrNotFound.
	Get(ctx context.Context, id string) (Sandbox, error)
	// List returns sandboxes in id order and, when more remain, the cursor
	// to pass as After for the next page.
	List(ctx context.Context, opts ListOptions) ([]Sandbox, string, error)
	// Update replaces s only when the stored resource version is expect,
	// else ErrVersionMismatch. ID, Owner, Driver and CreatedAt cannot change.
	Update(ctx context.Context, s Sandbox, expect int64) (Sandbox, error)
	// Delete removes the sandbox only when its resource version is expect.
	Delete(ctx context.Context, id string, expect int64) error

	IdempotencyStore
}

// ListOptions filters and pages a List.
type ListOptions struct {
	Selector Selector
	// State and Owner, when set, must match exactly.
	State State
	Owner string
	// Labels, when set, must each be on the sandbox with the same value.
	Labels map[string]string
	// Limit is the page size: DefaultLimit when zero, at most MaxLimit.
	Limit int
	// After is the cursor from the previous page.
	After string
}

const (
	DefaultLimit = 100
	MaxLimit     = 1000
)

// PageSize is o.Limit bounded as the contract says.
func (o ListOptions) PageSize() int {
	switch {
	case o.Limit <= 0:
		return DefaultLimit
	case o.Limit > MaxLimit:
		return MaxLimit
	}
	return o.Limit
}

// Matches reports whether s passes o's filters (not its paging).
func (o ListOptions) Matches(s Sandbox) bool {
	return (o.State == "" || s.State == o.State) && (o.Owner == "" || s.Owner == o.Owner) &&
		(len(o.Labels) == 0 || scoped(o.Labels, s.Labels)) && o.Selector.Matches(s.Labels)
}

// IdemState is where an idempotent request is.
type IdemState string

const (
	IdemPending IdemState = "pending"
	IdemDone    IdemState = "done"
)

// IdemRecord is one claimed idempotency key.
type IdemRecord struct {
	// Scope is the principal, method and path the key is valid for.
	Scope       string    `json:"scope"`
	Key         string    `json:"key"`
	RequestHash string    `json:"request_hash"`
	State       IdemState `json:"state"`
	Status      int       `json:"status,omitempty"`
	Body        []byte    `json:"body,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
	// Token names the claim. Only the request holding it may renew,
	// complete or release the record; a record read back never carries it.
	Token string `json:"-"`
}

// IdempotencyStore keeps idempotency records. Records past ExpiresAt are
// treated as absent.
type IdempotencyStore interface {
	// Claim stores rec when no live record holds its scope and key, and
	// reports claimed. Otherwise it returns the live record. Two concurrent
	// claims of one key never both succeed.
	Claim(ctx context.Context, rec IdemRecord, now time.Time) (existing IdemRecord, claimed bool, err error)
	// Renew moves a pending record's expiry to expires, while token still
	// holds it; ErrNotFound when it does not.
	Renew(ctx context.Context, scope, key, token string, expires time.Time) error
	// Complete stores the response for a pending record token holds and
	// keeps it until expires; ErrNotFound when token does not hold it.
	Complete(ctx context.Context, scope, key, token string, status int, body []byte, expires time.Time) error
	// Release removes a pending record token holds, so the request can be
	// tried again. A record another claim holds is left alone.
	Release(ctx context.Context, scope, key, token string) error
}

// mutateRetries bounds how often Mutate re-reads after losing a race.
const mutateRetries = 5

// Mutate applies fn to the current sandbox and writes the result guarded by
// the version it read, so a lost update is impossible. With an If-Match
// header the caller decided on one version, so losing a race to another
// writer is ErrPreconditionFailed; without one, Mutate reads again and
// retries. fn may run more than once, so it only changes the sandbox it is
// given; an error from it leaves the stored sandbox unchanged.
func Mutate(ctx context.Context, st Store, id, ifMatch string, fn func(*Sandbox) error) (Sandbox, error) {
	for attempt := 0; ; attempt++ {
		cur, err := st.Get(ctx, id)
		if err != nil {
			return Sandbox{}, err
		}
		if err := CheckIfMatch(ifMatch, cur.ResourceVersion); err != nil {
			return Sandbox{}, err
		}
		next := cur.Clone()
		if err := fn(&next); err != nil {
			return Sandbox{}, err
		}
		out, err := st.Update(ctx, next, cur.ResourceVersion)
		if !errors.Is(err, ErrVersionMismatch) {
			return out, err
		}
		if strings.TrimSpace(ifMatch) != "" && strings.TrimSpace(ifMatch) != "*" {
			return Sandbox{}, fmt.Errorf("%w: %s", ErrPreconditionFailed, err.Error())
		}
		if attempt == mutateRetries {
			return Sandbox{}, err
		}
	}
}
