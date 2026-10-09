package controlplane

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Memory is a Store in memory, for tests and a single process.
type Memory struct {
	mu        sync.Mutex
	rv        int64
	sandboxes map[string]Sandbox
	idem      map[idemKey]IdemRecord
	swept     time.Time
}

type idemKey struct{ scope, key string }

// NewMemory returns an empty store.
func NewMemory() *Memory {
	return &Memory{sandboxes: map[string]Sandbox{}, idem: map[idemKey]IdemRecord{}}
}

func (m *Memory) Create(_ context.Context, s Sandbox) (Sandbox, error) {
	if !ValidID(s.ID) {
		return Sandbox{}, fmt.Errorf("%w: sandbox id %q", ErrInvalid, s.ID)
	}
	if !s.State.Valid() {
		return Sandbox{}, fmt.Errorf("%w: state %q", ErrInvalid, s.State)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sandboxes[s.ID]; ok {
		return Sandbox{}, fmt.Errorf("%w: sandbox %s", ErrExists, s.ID)
	}
	m.rv++
	s = s.Clone()
	s.ResourceVersion = m.rv
	m.sandboxes[s.ID] = s
	return s.Clone(), nil
}

func (m *Memory) Get(_ context.Context, id string) (Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sandboxes[id]
	if !ok {
		return Sandbox{}, fmt.Errorf("%w: sandbox %s", ErrNotFound, id)
	}
	return s.Clone(), nil
}

func (m *Memory) List(_ context.Context, o ListOptions) ([]Sandbox, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.sandboxes))
	for id := range m.sandboxes {
		if id > o.After {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	size := o.PageSize()
	var out []Sandbox
	for _, id := range ids {
		s := m.sandboxes[id]
		if !o.Matches(s) {
			continue
		}
		if len(out) == size {
			return out, out[len(out)-1].ID, nil
		}
		out = append(out, s.Clone())
	}
	return out, "", nil
}

func (m *Memory) Update(_ context.Context, s Sandbox, expect int64) (Sandbox, error) {
	if !s.State.Valid() {
		return Sandbox{}, fmt.Errorf("%w: state %q", ErrInvalid, s.State)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.sandboxes[s.ID]
	if !ok {
		return Sandbox{}, fmt.Errorf("%w: sandbox %s", ErrNotFound, s.ID)
	}
	if cur.ResourceVersion != expect {
		return Sandbox{}, fmt.Errorf("%w: sandbox %s is at %d, not %d", ErrVersionMismatch, s.ID, cur.ResourceVersion, expect)
	}
	if err := immutable(cur, s); err != nil {
		return Sandbox{}, err
	}
	m.rv++
	s = s.Clone()
	s.ResourceVersion = m.rv
	m.sandboxes[s.ID] = s
	return s.Clone(), nil
}

// immutable refuses an update that changes what never changes after create.
func immutable(cur, next Sandbox) error {
	switch {
	case next.Owner != cur.Owner:
		return fmt.Errorf("%w: owner cannot change", ErrInvalid)
	case next.Driver != cur.Driver:
		return fmt.Errorf("%w: driver cannot change", ErrInvalid)
	case !next.CreatedAt.Equal(cur.CreatedAt):
		return fmt.Errorf("%w: created_at cannot change", ErrInvalid)
	case next.Generation < cur.Generation:
		return fmt.Errorf("%w: generation cannot go down", ErrInvalid)
	}
	return nil
}

func (m *Memory) Delete(_ context.Context, id string, expect int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.sandboxes[id]
	if !ok {
		return fmt.Errorf("%w: sandbox %s", ErrNotFound, id)
	}
	if cur.ResourceVersion != expect {
		return fmt.Errorf("%w: sandbox %s is at %d, not %d", ErrVersionMismatch, id, cur.ResourceVersion, expect)
	}
	delete(m.sandboxes, id)
	return nil
}

func (m *Memory) Claim(_ context.Context, rec IdemRecord, now time.Time) (IdemRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if now.Sub(m.swept) > time.Minute {
		for k, r := range m.idem {
			if !now.Before(r.ExpiresAt) {
				delete(m.idem, k)
			}
		}
		m.swept = now
	}
	k := idemKey{rec.Scope, rec.Key}
	if cur, ok := m.idem[k]; ok && now.Before(cur.ExpiresAt) {
		cur.Body = slices.Clone(cur.Body)
		return cur, false, nil
	}
	rec.State, rec.Status, rec.Body = IdemPending, 0, nil
	m.idem[k] = rec
	return rec, true, nil
}

func (m *Memory) Complete(_ context.Context, scope, key string, status int, body []byte, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := idemKey{scope, key}
	cur, ok := m.idem[k]
	if !ok || cur.State != IdemPending {
		return fmt.Errorf("%w: no pending request for idempotency key %q", ErrNotFound, key)
	}
	cur.State, cur.Status, cur.Body, cur.ExpiresAt = IdemDone, status, slices.Clone(body), expires
	m.idem[k] = cur
	return nil
}

func (m *Memory) Release(_ context.Context, scope, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := idemKey{scope, key}
	if cur, ok := m.idem[k]; ok && cur.State == IdemPending {
		delete(m.idem, k)
	}
	return nil
}
