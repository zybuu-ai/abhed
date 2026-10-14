package controlplane

import (
	"fmt"
	"slices"
	"strings"
)

// ReservedPrefix marks label keys only the server sets.
const ReservedPrefix = "abhed.dev/"

// MaxLabels bounds the labels on one sandbox.
const MaxLabels = 64

// ValidateLabels checks every key and value, and refuses keys under
// ReservedPrefix unless reserved is true.
func ValidateLabels(labels map[string]string, reserved bool) error {
	if len(labels) > MaxLabels {
		return fmt.Errorf("%w: %d labels, at most %d", ErrInvalid, len(labels), MaxLabels)
	}
	for k, v := range labels {
		if err := validKey(k); err != nil {
			return err
		}
		if !reserved && strings.HasPrefix(k, ReservedPrefix) {
			return fmt.Errorf("%w: label %q is reserved", ErrInvalid, k)
		}
		if !validValue(v) {
			return fmt.Errorf("%w: label %s has value %q", ErrInvalid, k, v)
		}
	}
	return nil
}

func validKey(k string) error {
	name := k
	if prefix, rest, ok := strings.Cut(k, "/"); ok {
		if !validPrefix(prefix) {
			return fmt.Errorf("%w: label key prefix %q", ErrInvalid, prefix)
		}
		name = rest
	}
	if name == "" || !validValue(name) {
		return fmt.Errorf("%w: label key %q", ErrInvalid, k)
	}
	return nil
}

// validValue: up to 63 of [A-Za-z0-9._-], starting and ending alphanumeric.
func validValue(v string) bool {
	if len(v) > 63 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && ((c != '.' && c != '_' && c != '-') || i == 0 || i == len(v)-1) {
			return false
		}
	}
	return true
}

// validPrefix: a DNS subdomain, lowercase.
func validPrefix(p string) bool {
	if p == "" || len(p) > 253 {
		return false
	}
	for _, part := range strings.Split(p, ".") {
		if part == "" || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// Operator is how a requirement compares a label.
type Operator string

const (
	OpEquals       Operator = "="
	OpNotEquals    Operator = "!="
	OpIn           Operator = "in"
	OpNotIn        Operator = "notin"
	OpExists       Operator = "exists"
	OpDoesNotExist Operator = "!"
)

// Requirement is one clause of a selector.
type Requirement struct {
	Key    string
	Op     Operator
	Values []string
}

// Matches reports whether labels satisfy r. != and notin also match a
// sandbox without the key.
func (r Requirement) Matches(labels map[string]string) bool {
	v, has := labels[r.Key]
	switch r.Op {
	case OpEquals:
		return has && v == r.Values[0]
	case OpNotEquals:
		return !has || v != r.Values[0]
	case OpIn:
		return has && slices.Contains(r.Values, v)
	case OpNotIn:
		return !has || !slices.Contains(r.Values, v)
	case OpExists:
		return has
	case OpDoesNotExist:
		return !has
	}
	return false
}

// Selector is requirements that must all hold. The empty selector matches
// every sandbox.
type Selector []Requirement

// Matches reports whether labels satisfy every requirement.
func (s Selector) Matches(labels map[string]string) bool {
	for _, r := range s {
		if !r.Matches(labels) {
			return false
		}
	}
	return true
}

// ParseSelector reads "k=v,k!=v,k,!k,k in (a,b),k notin (a,b)". Anything it
// cannot read is an error, never a selector that matches everything.
func ParseSelector(s string) (Selector, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out Selector
	for _, clause := range splitClauses(s) {
		r, err := parseRequirement(strings.TrimSpace(clause))
		if err != nil {
			return nil, fmt.Errorf("%w: selector %q: %w", ErrInvalid, s, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// splitClauses splits on commas outside parentheses.
func splitClauses(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

func parseRequirement(c string) (Requirement, error) {
	if c == "" {
		return Requirement{}, fmt.Errorf("empty requirement")
	}
	if strings.HasPrefix(c, "!") && !strings.Contains(c, "=") {
		k := strings.TrimSpace(c[1:])
		return Requirement{Key: k, Op: OpDoesNotExist}, validKey(k)
	}
	if open := strings.IndexByte(c, '('); open >= 0 {
		head, body := strings.Fields(c[:open]), c[open+1:]
		if len(head) != 2 || !strings.HasSuffix(body, ")") || strings.ContainsAny(body[:len(body)-1], "()") {
			return Requirement{}, fmt.Errorf("cannot read %q", c)
		}
		op := Operator(head[1])
		if op != OpIn && op != OpNotIn {
			return Requirement{}, fmt.Errorf("unknown operator %q", head[1])
		}
		if strings.TrimSpace(body[:len(body)-1]) == "" {
			return Requirement{}, fmt.Errorf("%s with no values", op)
		}
		var vals []string
		for _, v := range strings.Split(body[:len(body)-1], ",") {
			v = strings.TrimSpace(v)
			if !validValue(v) {
				return Requirement{}, fmt.Errorf("value %q", v)
			}
			vals = append(vals, v)
		}
		return Requirement{Key: head[0], Op: op, Values: vals}, validKey(head[0])
	}
	for _, sep := range []struct {
		s  string
		op Operator
	}{{"!=", OpNotEquals}, {"==", OpEquals}, {"=", OpEquals}} {
		if k, v, ok := strings.Cut(c, sep.s); ok {
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if !validValue(v) || strings.ContainsAny(v, "=!") {
				return Requirement{}, fmt.Errorf("value %q", v)
			}
			return Requirement{Key: k, Op: sep.op, Values: []string{v}}, validKey(k)
		}
	}
	if strings.ContainsAny(c, " \t") {
		return Requirement{}, fmt.Errorf("cannot read %q", c)
	}
	return Requirement{Key: c, Op: OpExists}, validKey(c)
}
