package policy

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Rule lists, as a person names them.
const (
	ListAllow = "allow"
	ListAsk   = "ask"
	ListDeny  = "deny"
)

// Overlay holds the rules a person adds while a session runs, apart from the
// configured ones, so they end with the session. Its rules are evaluated in
// the same order as the configured ones: a session allow sits with the allow
// rules, after deny rules, destructive commands, ask rules and the mode, so
// it can only approve what would otherwise ask.
//
// Pinned deny rules are kept when the session's rules are cleared; they
// narrow what a directory added part way through a session allows.
type Overlay struct {
	mu                sync.RWMutex
	deny, ask, allow  []Rule
	pinned            []Rule
	pinnedDescription []string
}

// Add parses pattern and adds it to list; adding a rule already there
// changes nothing and reports false.
func (o *Overlay) Add(list, pattern string) (bool, error) {
	r, err := ParseRule(pattern)
	if err != nil {
		return false, err
	}
	r.session = true
	o.mu.Lock()
	defer o.mu.Unlock()
	dst, err := o.list(list)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(*dst, func(have Rule) bool { return have.raw == r.raw }) {
		return false, nil
	}
	*dst = append(*dst, r)
	return true, nil
}

// Remove takes pattern out of list and reports whether it was there.
func (o *Overlay) Remove(list, pattern string) (bool, error) {
	pattern = strings.TrimSpace(pattern)
	o.mu.Lock()
	defer o.mu.Unlock()
	dst, err := o.list(list)
	if err != nil {
		return false, err
	}
	n := len(*dst)
	*dst = slices.DeleteFunc(*dst, func(r Rule) bool { return r.raw == pattern })
	return len(*dst) != n, nil
}

func (o *Overlay) list(name string) (*[]Rule, error) {
	switch name {
	case ListAllow:
		return &o.allow, nil
	case ListAsk:
		return &o.ask, nil
	case ListDeny:
		return &o.deny, nil
	}
	return nil, fmt.Errorf("unknown rule list %q: want allow, ask or deny", name)
}

// Clear drops the session's rules, as a new conversation starts; pinned
// rules stay.
func (o *Overlay) Clear() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deny, o.ask, o.allow = nil, nil, nil
}

// Pin adds deny rules that Clear keeps, with why, for a listing.
func (o *Overlay) Pin(why string, patterns ...string) error {
	rules := make([]Rule, 0, len(patterns))
	for _, p := range patterns {
		r, err := ParseRule(p)
		if err != nil {
			return err
		}
		r.session = true
		rules = append(rules, r)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pinned = append(o.pinned, rules...)
	for range rules {
		o.pinnedDescription = append(o.pinnedDescription, why)
	}
	return nil
}

// SessionRules are the session's own rules by list, as written.
func (o *Overlay) SessionRules() (deny, ask, allow []string) {
	if o == nil {
		return nil, nil, nil
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return rawOf(o.deny), rawOf(o.ask), rawOf(o.allow)
}

// PinnedRules are the kept deny rules, each with why it was pinned.
func (o *Overlay) PinnedRules() (rules, why []string) {
	if o == nil {
		return nil, nil
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return rawOf(o.pinned), slices.Clone(o.pinnedDescription)
}

func rawOf(rules []Rule) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.raw
	}
	return out
}

// snapshot is the overlay's rules, safe to read while the session changes them.
func (o *Overlay) snapshot() (deny, ask, allow []Rule) {
	if o == nil {
		return nil, nil, nil
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return append(slices.Clone(o.pinned), o.deny...), slices.Clone(o.ask), slices.Clone(o.allow)
}

// rules are the configured rules followed by the session's.
func (e *Engine) rules() (deny, ask, allow []Rule) {
	sd, sa, sl := e.Session.snapshot()
	if len(sd)+len(sa)+len(sl) == 0 {
		return e.Deny, e.Ask, e.Allow
	}
	return append(slices.Clone(e.Deny), sd...), append(slices.Clone(e.Ask), sa...), append(slices.Clone(e.Allow), sl...)
}

// DenyRules are every deny rule in force: the configured ones and the
// session's, for checks made outside Evaluate.
func (e *Engine) DenyRules() []Rule {
	deny, _, _ := e.rules()
	return deny
}

// IsBroad reports whether an allow rule would approve every call to a tool:
// a bare tool name, any tool (*), or a pattern of only wildcards and slashes,
// such as bash(*) or write(**).
func IsBroad(pattern string) bool {
	r, err := ParseRule(pattern)
	if err != nil {
		return false
	}
	return r.tool == "*" || r.pattern == nil || strings.Trim(r.glob, "*/") == ""
}

// MatchesCall reports whether any of rules matches a call the way a deny rule
// would: each part of a bash chain, a path in every spelling against the
// roots, and NFC. When the call cannot be read that far it reports true.
func (e *Engine) MatchesCall(rules []Rule, tool string, args json.RawMessage) bool {
	key, subject, err := subjectOf(args)
	if err != nil {
		return true
	}
	subjects, match := []string{subject}, Rule.matchesAny
	switch {
	case tool == "bash":
		var complete bool
		if subjects, complete = commandSegments(subject); !complete {
			return true
		}
		subjects = append(subjects, subject)
	case key == "path":
		subjects, _ = e.pathSubjects(subject)
		match = Rule.matchesPathAny
	}
	for _, r := range rules {
		if match(r, tool, subjects) {
			return true
		}
	}
	return false
}
