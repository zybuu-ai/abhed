package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/zybuu-ai/abhed/internal/secrets"
)

// SubjectKeys are the arguments policy reads a call's subject from, most
// security-relevant first. url comes last so a tool that already had a
// subject keeps it; web_fetch has only a url.
var SubjectKeys = []string{"command", "path", "pattern", "action", "resource", "host", "namespace", "name", "url"}

// ErrMalformedArgs marks arguments refused before any rule or tool reads them.
var ErrMalformedArgs = errors.New("malformed arguments")

// FixedArgs marks a tool whose arguments are exactly its schema's properties.
type FixedArgs interface {
	FixedArgs()
}

// The native tools decode into structs whose fields are their schemas' properties.
func (Bash) FixedArgs()  {}
func (Read) FixedArgs()  {}
func (Write) FixedArgs() {}
func (Edit) FixedArgs()  {}
func (Glob) FixedArgs()  {}
func (Grep) FixedArgs()  {}
func (Todo) FixedArgs()  {}

const maxArgsDepth = 1000

// DecodeArgs decodes tool arguments strictly: one JSON object, each key once
// even when case is ignored, at every depth, and nothing after it. Empty and
// null are the empty object.
func DecodeArgs(raw json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := strictValue(dec, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedArgs, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: data after the arguments object", ErrMalformedArgs)
	}
	switch m := v.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return m, nil
	}
	return nil, fmt.Errorf("%w: the arguments must be a JSON object, not %s", ErrMalformedArgs, jsonKind(v))
}

// jsonKind names a decoded value's JSON type for a message.
func jsonKind(v any) string {
	switch v.(type) {
	case []any:
		return "a list"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	}
	return "a number"
}

func strictValue(dec *json.Decoder, depth int) (any, error) {
	if depth > maxArgsDepth {
		return nil, errors.New("nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, isDelim := tok.(json.Delim)
	if !isDelim {
		return tok, nil
	}
	switch d {
	case '{':
		m := map[string]any{}
		folded := map[string]string{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, _ := kt.(string)
			if _, dup := m[k]; dup {
				return nil, fmt.Errorf("duplicate key %q", clipKey(k))
			}
			fk := FoldKey(k)
			if prev, clash := folded[fk]; clash {
				return nil, fmt.Errorf("keys %q and %q differ only in case", clipKey(prev), clipKey(k))
			}
			folded[fk] = k
			if m[k], err = strictValue(dec, depth+1); err != nil {
				return nil, err
			}
		}
		_, err := dec.Token()
		return m, err
	case '[':
		a := []any{}
		for dec.More() {
			v, err := strictValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := dec.Token()
		return a, err
	}
	return nil, fmt.Errorf("unexpected %v", d)
}

// FoldKey folds a key the way encoding/json matches struct fields, so two
// keys fold alike exactly when one struct field would take either.
func FoldKey(k string) string {
	var b strings.Builder
	for _, r := range k {
		for {
			n := unicode.SimpleFold(r)
			if n <= r {
				r = n
				break
			}
			r = n
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Lookup returns the value of the key that folds to name, as a struct field would.
func Lookup(m map[string]any, name string) (any, bool) {
	if v, ok := m[name]; ok {
		return v, true
	}
	want := FoldKey(name)
	for k, v := range m {
		if FoldKey(k) == want {
			return v, true
		}
	}
	return nil, false
}

// CanonicalArgs checks a call's arguments against its tool and returns the one
// encoding that policy judges, the approver sees, the record keeps and the tool runs.
// A fixed tool's unknown keys that no reader could take for another are dropped and named.
func CanonicalArgs(t Tool, raw json.RawMessage) (canon json.RawMessage, dropped []string, err error) {
	m, err := DecodeArgs(raw)
	if err != nil {
		return nil, nil, err
	}
	props, closed := schemaProps(t.Schema())
	_, fixed := t.(FixedArgs)
	names := make([]string, 0, len(props))
	for p := range props {
		names = append(names, p)
	}
	sort.Strings(names)
	list := strings.Join(names, ", ")
	for k := range m {
		if props[k] {
			continue
		}
		fk := FoldKey(k)
		switch {
		case foldsToAny(fk, names):
			return nil, nil, fmt.Errorf("%w: %q must be spelled exactly as in the schema (arguments: %s)", ErrMalformedArgs, clipKey(k), list)
		case (fixed || len(props) > 0) && foldsToAny(fk, SubjectKeys):
			return nil, nil, fmt.Errorf("%w: %q is not an argument of %s (arguments: %s)", ErrMalformedArgs, clipKey(k), t.Name(), list)
		case fixed:
			dropped = append(dropped, k)
			delete(m, k)
		case closed:
			return nil, nil, fmt.Errorf("%w: %q is not an argument of %s (arguments: %s)", ErrMalformedArgs, clipKey(k), t.Name(), list)
		}
	}
	// Rules read the first subject argument as text; one given as a number or
	// a list would pass them by, so it must be a string unless the schema says not.
	for _, k := range SubjectKeys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		if _, isStr := v.(string); !isStr && schemaType(t.Schema(), k) == "string" {
			return nil, nil, fmt.Errorf("%w: %q must be a string", ErrMalformedArgs, k)
		}
	}
	sort.Strings(dropped)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrMalformedArgs, err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), dropped, nil
}

// clipKey shortens a model-chosen key before it is echoed back.
func clipKey(k string) string {
	if r := []rune(k); len(r) > 64 {
		return string(r[:64]) + "…"
	}
	return k
}

func foldsToAny(fk string, names []string) bool {
	for _, n := range names {
		if FoldKey(n) == fk {
			return true
		}
	}
	return false
}

// schemaProps reads a schema's top-level property names, and whether it refuses others.
// schemaType is the type the schema gives property key: "string" where it
// names none, or gives no schema for it, since rules read it as text.
func schemaType(schema json.RawMessage, key string) string {
	var s struct {
		Properties map[string]struct {
			Type json.RawMessage `json:"type"`
		} `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return "string"
	}
	p, ok := s.Properties[key]
	var typ string
	if !ok || len(p.Type) == 0 || json.Unmarshal(p.Type, &typ) != nil || typ == "" {
		return "string"
	}
	return typ
}

func schemaProps(schema json.RawMessage) (map[string]bool, bool) {
	var s struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties json.RawMessage            `json:"additionalProperties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return nil, false
	}
	props := make(map[string]bool, len(s.Properties))
	for k := range s.Properties {
		props[k] = true
	}
	return props, strings.TrimSpace(string(s.AdditionalProperties)) == "false"
}

// SecretArgs marks a tool that takes the name of a stored secret in each of
// the named string arguments. Each name is put to its own secret(NAME) rule.
type SecretArgs interface {
	SecretArgs() []string
}

// WithheldSecretArg replaces a value given where a secret's name belongs.
const WithheldSecretArg = "[withheld: not a secret name]"

// SecretPaths marks a tool that takes secret names deeper in its arguments:
// each path is a list of keys, "*" standing for any key or list index, and
// the string it reaches is put to its own secret(NAME) rule.
type SecretPaths interface {
	SecretPaths() [][]string
}

// SecretWithholder is a tool whose arguments can carry a secret's value
// somewhere other than a secret's name, such as a Secret in a manifest:
// WithholdSecrets returns the arguments with any such value withheld, or
// them unchanged.
type SecretWithholder interface {
	WithholdSecrets(args json.RawMessage) json.RawMessage
}

// SecretNames returns the secret names a call to t asks for.
func SecretNames(t Tool, args json.RawMessage) []string {
	sa, ok := t.(SecretArgs)
	sp, deep := t.(SecretPaths)
	if !ok && !deep {
		return nil
	}
	m, err := DecodeArgs(args)
	if err != nil {
		return nil
	}
	var out []string
	if ok {
		for _, k := range sa.SecretArgs() {
			if v, ok := m[k].(string); ok && v != "" {
				out = append(out, v)
			}
		}
	}
	if deep {
		for _, p := range sp.SecretPaths() {
			walkPath(m, p, func(v any, _ func(any)) {
				if s, ok := v.(string); ok && s != "" {
					out = append(out, s)
				}
			})
		}
	}
	return out
}

// walkPath calls f with each value path reaches in v, and a setter for it.
func walkPath(v any, path []string, f func(v any, set func(any))) {
	if len(path) == 0 {
		return
	}
	key, rest := path[0], path[1:]
	visit := func(child any, set func(any)) {
		if len(rest) == 0 {
			f(child, set)
		} else {
			walkPath(child, rest, f)
		}
	}
	switch c := v.(type) {
	case map[string]any:
		if key == "*" {
			for k, child := range c {
				visit(child, func(n any) { c[k] = n })
			}
		} else if child, ok := c[key]; ok {
			visit(child, func(n any) { c[key] = n })
		}
	case []any:
		if key == "*" {
			for i, child := range c {
				visit(child, func(n any) { c[i] = n })
			}
		}
	}
}

// WithholdSecretValues replaces anything but a secret's name in t's secret
// arguments. A model that puts a credential where its name belongs would
// otherwise have it judged, shown for approval and recorded, and the record is
// append-only. Arguments with nothing to withhold come back unchanged.
func WithholdSecretValues(t Tool, canon json.RawMessage) json.RawMessage {
	if sw, ok := t.(SecretWithholder); ok {
		canon = sw.WithholdSecrets(canon)
	}
	sa, ok := t.(SecretArgs)
	sp, deep := t.(SecretPaths)
	if !ok && !deep {
		return canon
	}
	m, err := DecodeArgs(canon)
	if err != nil {
		return canon
	}
	changed := false
	if ok {
		for _, k := range sa.SecretArgs() {
			v, found := m[k]
			if !found {
				continue
			}
			if s, isStr := v.(string); isStr && (s == "" || secrets.ValidName(s)) {
				continue
			}
			m[k] = WithheldSecretArg
			changed = true
		}
	}
	if deep {
		for _, p := range sp.SecretPaths() {
			walkPath(m, p, func(v any, set func(any)) {
				if s, isStr := v.(string); isStr && (s == "" || secrets.ValidName(s)) {
					return
				}
				set(WithheldSecretArg)
				changed = true
			})
		}
	}
	if !changed {
		return canon
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return json.RawMessage(`{}`)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}
