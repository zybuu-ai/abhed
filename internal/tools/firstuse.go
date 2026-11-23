package tools

import (
	"encoding/json"
	"sync"
)

// FirstUse is what a call needs before it may reach something the person's
// own machine set up, such as a kubeconfig context or an ~/.ssh/config host:
// the person's yes, once.
type FirstUse struct {
	// Scope is what "always" keeps, such as k8s_context(prod); it never
	// equals a scope a call of any tool offers, so no remembered scope for a
	// call can answer it, nor it a call.
	Scope string
	// What names it for the person: "kubeconfig context prod at https://…".
	What string
	// Reason says why the call asks.
	Reason string
	// Kind, Name and Identity are the grant kept when the person says always.
	Kind, Name, Identity string
}

// FirstUser is a tool whose call may reach what the person's own machine set
// up. Its first call to each one asks, in every mode but bypass, even where
// the call would otherwise run unasked, until the person approves one. An
// approval covers the rest of the session; "always" keeps the grant in the
// person's own state for later sessions.
type FirstUser interface {
	// FirstUse returns what the call needs, and false when it needs nothing:
	// it reaches nothing of the person's machine, or that is granted.
	FirstUse(sess *Session, args json.RawMessage) (FirstUse, bool)
	// KeepFirstUse keeps g for the person, after they chose always.
	KeepFirstUse(g FirstUse) error
}

type firstUseKey struct{}

type firstUses struct {
	mu   sync.Mutex
	seen map[string]bool
}

func firstUseKeyOf(kind, name, identity string) string {
	return kind + "\x00" + name + "\x00" + identity
}

// FirstUsed reports whether the session already approved g's use.
func FirstUsed(sess *Session, kind, name, identity string) bool {
	f, _ := sess.Scoped(firstUseKey{}, nil).(*firstUses)
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[firstUseKeyOf(kind, name, identity)]
}

// MarkFirstUsed notes that the session approved g's use.
func MarkFirstUsed(sess *Session, g FirstUse) {
	f, _ := sess.Scoped(firstUseKey{}, func() any { return &firstUses{seen: map[string]bool{}} }).(*firstUses)
	if f == nil {
		return
	}
	f.mu.Lock()
	f.seen[firstUseKeyOf(g.Kind, g.Name, g.Identity)] = true
	f.mu.Unlock()
}

// BoundSecrets is a tool that reads stored secrets a kept grant binds to
// what the call reaches, such as the token a person pasted for a cluster,
// rather than secrets the call names. They need no allow rule, as the
// person bound them when they approved the grant; a deny rule still refuses.
type BoundSecrets interface {
	BoundSecrets(sess *Session, args json.RawMessage) []string
}

// FirstUseScope is the scope a first use of kind offers for name.
func FirstUseScope(kind, name string) string {
	switch kind {
	case "ssh-host":
		return "ssh_host(" + name + ")"
	case "k8s-login":
		return "k8s_cluster(" + name + ")"
	}
	return "k8s_context(" + name + ")"
}

// IsFirstUseScope reports a scope FirstUseScope made. It is kept by the
// person's grants, never restored from a session's record as a call's scope.
func IsFirstUseScope(scope string) bool {
	for _, p := range []string{"ssh_host(", "k8s_context(", "k8s_cluster("} {
		if len(scope) > len(p) && scope[:len(p)] == p {
			return true
		}
	}
	return false
}
