package driver

import (
	"sort"
	"sync"
)

// Registry maps driver names to drivers.
type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{drivers: map[string]Driver{}} }

// Register adds d under its own name, after Check. A name is registered once.
func (r *Registry) Register(d Driver) error {
	if d == nil {
		return Errorf(ErrInvalid, "nil driver")
	}
	if err := Check(d); err != nil {
		return err
	}
	name := d.Info().Name
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.drivers[name]; dup {
		return Errorf(ErrExists, "driver %s is already registered", name)
	}
	r.drivers[name] = d
	return nil
}

// Get returns the driver registered as name, or ErrNotFound.
func (r *Registry) Get(name string) (Driver, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drivers[name]
	if !ok {
		return nil, Errorf(ErrNotFound, "no driver named %q is registered", name)
	}
	return d, nil
}

// Names lists the registered drivers, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.drivers))
	for n := range r.drivers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
