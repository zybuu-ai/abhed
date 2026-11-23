package tools

import (
	"fmt"
	"sync"
	"time"
)

// Pasted is a credential a person pasted into a message. The model and the
// record get a placeholder ([pasted:P1]); the value is held here, in the
// session's memory only, until a tool the person approves uses it or the
// session ends.
type Pasted struct {
	ID    string
	Kind  string // what it looks like: "an OpenShift token", "a bearer token", …
	Value string
	// Server is the API server pasted with it (oc login --server=…), or "".
	Server string
	// Namespace is the namespace pasted with it (-n, --namespace), or "".
	Namespace string
	At        time.Time
}

type pastedKey struct{}

type pastedSet struct {
	mu    sync.Mutex
	items map[string]Pasted
	n     int
}

// HoldPasted keeps p for the session and returns its id. The same value
// pasted again keeps its first id. "" when there is no session to hold it.
func HoldPasted(sess *Session, p Pasted) string {
	set, _ := sess.Scoped(pastedKey{}, func() any { return &pastedSet{items: map[string]Pasted{}} }).(*pastedSet)
	if set == nil {
		return ""
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	for id, have := range set.items {
		if have.Value == p.Value {
			if have.Server == "" && p.Server != "" {
				have.Server = p.Server
				set.items[id] = have
			}
			return id
		}
	}
	set.n++
	p.ID = fmt.Sprintf("P%d", set.n)
	if p.At.IsZero() {
		p.At = time.Now()
	}
	set.items[p.ID] = p
	return p.ID
}

// PastedValue returns the credential held under id.
func PastedValue(sess *Session, id string) (Pasted, bool) {
	set, _ := sess.Scoped(pastedKey{}, nil).(*pastedSet)
	if set == nil {
		return Pasted{}, false
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	p, ok := set.items[id]
	return p, ok
}

// DropPasted forgets the credential held under id, once it is stored.
func DropPasted(sess *Session, id string) {
	set, _ := sess.Scoped(pastedKey{}, nil).(*pastedSet)
	if set == nil {
		return
	}
	set.mu.Lock()
	delete(set.items, id)
	set.mu.Unlock()
}

// PastedValues are the values the session holds, for redaction.
func PastedValues(sess *Session) []string {
	set, _ := sess.Scoped(pastedKey{}, nil).(*pastedSet)
	if set == nil {
		return nil
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	out := make([]string, 0, len(set.items))
	for _, p := range set.items {
		out = append(out, p.Value)
	}
	return out
}
