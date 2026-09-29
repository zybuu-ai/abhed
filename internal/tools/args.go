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
)

// SubjectKeys are the arguments policy reads a call's subject from, most
// security-relevant first.
var SubjectKeys = []string{"command", "path", "pattern", "action", "resource", "host", "namespace", "name"}

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
		return nil, fmt.Errorf("%w: %v", ErrMalformedArgs, err)
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
	return nil, fmt.Errorf("%w: the arguments must be a JSON object", ErrMalformedArgs)
}

func strictValue(dec *json.Decoder, depth int) (any, error) {
	if depth > maxArgsDepth {
		return nil, errors.New("nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch d := tok.(type) {
	case json.Delim:
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
					return nil, fmt.Errorf("duplicate key %q", k)
				}
				fk := FoldKey(k)
				if prev, clash := folded[fk]; clash {
					return nil, fmt.Errorf("keys %q and %q differ only in case", prev, k)
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
	return tok, nil
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
func CanonicalArgs(t Tool, raw json.RawMessage) (json.RawMessage, error) {
	m, err := DecodeArgs(raw)
	if err != nil {
		return nil, err
	}
	props, open := schemaProps(t.Schema())
	if _, ok := t.(FixedArgs); ok {
		open = false
	}
	names := make([]string, 0, len(props))
	for p := range props {
		names = append(names, p)
	}
	sort.Strings(names)
	for k := range m {
		if _, declared := props[k]; declared {
			continue
		}
		fk := FoldKey(k)
		switch {
		case !open:
			return nil, fmt.Errorf("%w: %q is not an argument of %s (arguments: %s)", ErrMalformedArgs, k, t.Name(), strings.Join(names, ", "))
		case foldsToAny(fk, names):
			return nil, fmt.Errorf("%w: %q must be spelled exactly as in the schema (arguments: %s)", ErrMalformedArgs, k, strings.Join(names, ", "))
		case len(props) > 0 && foldsToAny(fk, SubjectKeys):
			return nil, fmt.Errorf("%w: %q is not an argument of %s (arguments: %s)", ErrMalformedArgs, k, t.Name(), strings.Join(names, ", "))
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedArgs, err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func foldsToAny(fk string, names []string) bool {
	for _, n := range names {
		if FoldKey(n) == fk {
			return true
		}
	}
	return false
}

// schemaProps reads a schema's top-level property names, and whether it admits others.
func schemaProps(schema json.RawMessage) (map[string]bool, bool) {
	var s struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties json.RawMessage            `json:"additionalProperties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return nil, true
	}
	props := make(map[string]bool, len(s.Properties))
	for k := range s.Properties {
		props[k] = true
	}
	return props, strings.TrimSpace(string(s.AdditionalProperties)) != "false"
}
