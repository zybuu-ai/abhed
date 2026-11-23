package tools

import (
	"fmt"
	"sort"
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

// PasteNoticer is a tool told when its session holds a paste, such as
// k8s_login, offered on a machine with nothing to reach only from then on.
type PasteNoticer interface {
	NotePasted()
}

type pastedSet struct {
	mu    sync.Mutex
	items map[string]Pasted
	// spent are values a login stored: no longer usable, still withheld
	// wherever they appear again in this session.
	spent map[string]Pasted
	n     int
}

// HoldPasted keeps p for the session and returns its id. The same value
// pasted again keeps its id, unless it names another server: a corrected
// paste is held anew, so a login names the server the person meant and no
// earlier check of the other one carries over. "" when there is no session.
func HoldPasted(sess *Session, p Pasted) string {
	set, _ := sess.Scoped(pastedKey{}, func() any { return &pastedSet{items: map[string]Pasted{}} }).(*pastedSet)
	if set == nil {
		return ""
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	for id, have := range set.items {
		if have.Value != p.Value || p.Server != "" && have.Server != "" && have.Server != p.Server {
			continue
		}
		if have.Server == "" && p.Server != "" {
			have.Server, have.Namespace = p.Server, p.Namespace
			set.items[id] = have
		}
		return id
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

// DropPasted makes the credential held under id unusable once it is stored;
// it is still withheld wherever it appears again in the session.
func DropPasted(sess *Session, id string) {
	set, _ := sess.Scoped(pastedKey{}, nil).(*pastedSet)
	if set == nil {
		return
	}
	set.mu.Lock()
	if p, ok := set.items[id]; ok {
		if set.spent == nil {
			set.spent = map[string]Pasted{}
		}
		set.spent[id] = p
		delete(set.items, id)
	}
	set.mu.Unlock()
}

// PastedHeld are the credentials the session holds, longest value first,
// for withholding them wherever they appear again.
func PastedHeld(sess *Session) []Pasted {
	set, _ := sess.Scoped(pastedKey{}, nil).(*pastedSet)
	if set == nil {
		return nil
	}
	set.mu.Lock()
	out := make([]Pasted, 0, len(set.items)+len(set.spent))
	for _, p := range set.items {
		out = append(out, p)
	}
	for _, p := range set.spent {
		out = append(out, p)
	}
	set.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Value) != len(out[j].Value) {
			return len(out[i].Value) > len(out[j].Value)
		}
		return out[i].ID < out[j].ID
	})
	return out
}
