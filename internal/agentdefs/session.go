package agentdefs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/frontmatter"
)

// MaxSession bounds the -agents JSON.
const MaxSession = 256 << 10

// ParseSession reads definitions given for one run as a JSON object of
// name to definition: the keys a file's header takes, with "prompt" for the
// role's instructions. Each is checked exactly as a file is. A definition
// that does not parse is reported in errs and left out; warns are keys ignored.
func ParseSession(raw []byte, models []string) (defs []*agent.Definition, warns []string, errs []error) {
	if len(raw) > MaxSession {
		return nil, nil, []error{fmt.Errorf("-agents is %d bytes; at most %d", len(raw), MaxSession)}
	}
	top, err := ordered(raw, false)
	if err != nil {
		return nil, nil, []error{fmt.Errorf("-agents: %w", err)}
	}
	var out []*agent.Definition
	for _, kv := range top {
		def, ws, err := parseSessionOne(kv.key, kv.val, models)
		for _, w := range ws {
			warns = append(warns, fmt.Sprintf("-agents %s: %s", config.Printable(kv.key), w))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("-agents %s refused: %w", config.Printable(kv.key), err))
			continue
		}
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, warns, errs
}

func parseSessionOne(name string, raw json.RawMessage, models []string) (*agent.Definition, []string, error) {
	fields, err := ordered(raw, true)
	if err != nil {
		return nil, nil, err
	}
	doc := &frontmatter.Document{}
	named := false
	for _, kv := range fields {
		if kv.key == "prompt" {
			if err := json.Unmarshal(kv.val, &doc.Body); err != nil {
				return nil, nil, errors.New(`"prompt" must be a string`)
			}
			continue
		}
		f, nested, err := fieldOf(kv.key, kv.val)
		if err != nil {
			return nil, nil, err
		}
		if keyOf(kv.key) == "name" {
			if f.Value != name {
				return nil, nil, fmt.Errorf("its name %q differs from its key", config.Printable(f.Value))
			}
			named = true
		}
		doc.Fields = append(doc.Fields, f)
		doc.Fields = append(doc.Fields, nested...)
	}
	if !named {
		doc.Fields = append(doc.Fields, frontmatter.Field{Key: "name", Value: name})
	}
	sum := sha256.Sum256(raw)
	return parseDoc(doc, "-agents "+name, hex.EncodeToString(sum[:]), agent.SourceSession, models)
}

// fieldOf is one JSON member as a header field: a string, number or boolean
// is a value, an array of strings a list, and an object a nested block whose
// keys are checked as a file's nested keys are.
func fieldOf(key string, raw json.RawMessage) (frontmatter.Field, []frontmatter.Field, error) {
	f := frontmatter.Field{Key: key}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return f, nil, err
	}
	switch x := v.(type) {
	case nil:
	case string:
		f.Value = x
	case bool:
		f.Value = strconv.FormatBool(x)
	case float64:
		f.Value = string(bytes.TrimSpace(raw))
	case []any:
		f.Kind, f.List = frontmatter.List, []string{}
		for _, it := range x {
			s, ok := it.(string)
			if !ok {
				return f, nil, fmt.Errorf("%q must be a list of strings", config.Printable(key))
			}
			f.List = append(f.List, s)
		}
	case map[string]any:
		f.Kind = frontmatter.Map
		var nested []frontmatter.Field
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			nested = append(nested, frontmatter.Field{Key: k, Nested: true})
		}
		return f, nested, nil
	}
	return f, nil, nil
}

type member struct {
	key string
	val json.RawMessage
}

// ordered reads a JSON object's members in order, refusing a key given twice,
// spelt alike when fold is set: decoding into a map would keep the last silently.
func ordered(raw []byte, fold bool) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("want a JSON object")
	}
	var out []member
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		id := key
		if fold {
			id = keyOf(key)
		}
		if seen[id] {
			return nil, fmt.Errorf("%q is set twice", config.Printable(key))
		}
		seen[id] = true
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		out = append(out, member{key, val})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the object")
	}
	return out, nil
}
