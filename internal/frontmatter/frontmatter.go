// Package frontmatter reads the header of a markdown file written as
// key: value lines between --- markers, the shape skills and agent
// definitions share.
//
// It is a narrow reader, not YAML: flat keys, quoted or plain values, folded
// and literal block scalars, and lists written inline ([a, b]) or as - items.
// A nested block under a key is reported as nested rather than parsed, so a
// caller can refuse a shape it does not understand. No YAML dependency: an
// air-gapped bundle should not carry a large parser for a few known keys.
package frontmatter

import (
	"errors"
	"strings"
)

// ErrMissing is content that does not start with a --- block.
var ErrMissing = errors.New("missing frontmatter")

// ErrUnclosed is a --- block with no closing ---.
var ErrUnclosed = errors.New("frontmatter is not closed with ---")

// Kind is the shape of a field's value.
type Kind int

const (
	// Scalar is a plain, quoted or block value.
	Scalar Kind = iota
	// List is an inline [a, b] list or a run of - items under the key.
	List
	// Map is a key whose value is a nested block of keys.
	Map
)

// Field is one key and its value, in the order written.
type Field struct {
	// Key is as written, trimmed.
	Key string
	// Value is the scalar text: quotes trimmed, a block scalar joined. For an
	// inline list it is the text as written; for - items and nested maps, "".
	Value string
	// List holds the items of a list, unquoted.
	List []string
	Kind Kind
	// Nested is a key inside another key's nested block, not a key of its own.
	Nested bool
	indent int
}

// Document is a parsed header and the text after it.
type Document struct {
	Fields []Field
	Body   string
}

// Top is the fields written at the top level, without nested ones.
func (d *Document) Top() []Field {
	out := make([]Field, 0, len(d.Fields))
	for _, f := range d.Fields {
		if !f.Nested {
			out = append(out, f)
		}
	}
	return out
}

// Parse splits content into its header fields and body. Line endings are
// normalised and the body is trimmed.
func Parse(content string) (*Document, error) {
	text := strings.ReplaceAll(content, "\r\n", "\n")
	text = strings.TrimLeft(text, " \t\n")
	if !strings.HasPrefix(text, "---") {
		return nil, ErrMissing
	}
	rest := text[3:]
	i := strings.Index(rest, "\n---")
	if i < 0 {
		return nil, ErrUnclosed
	}
	return &Document{Fields: parseFields(rest[:i]), Body: strings.TrimSpace(rest[i+4:])}, nil
}

func indentOf(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }

func parseFields(fm string) []Field {
	var out []Field
	lines := strings.Split(fm, "\n")
	base := -1  // the indent of top-level keys: that of the first key
	open := -1  // the top-level field an indented line belongs to
	owner := -1 // the field - items are added to
	for i := 0; i < len(lines); i++ {
		raw := lines[i]
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		indent := indentOf(raw)
		if line == "-" || strings.HasPrefix(line, "- ") {
			if owner >= 0 && out[owner].Value == "" && out[owner].Kind != Map {
				out[owner].Kind = List
				out[owner].List = append(out[owner].List, unquote(strings.TrimSpace(strings.TrimPrefix(line, "-"))))
			}
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if base < 0 {
			base = indent
		}
		nested := indent > base && open >= 0

		f := Field{Key: key, Nested: nested, indent: indent}
		// Block scalars: "description: >" (folded) or "|" (literal) put the
		// text on the following indented lines. A real skill used this and the
		// description parsed as ">" — one character, which the model would
		// never match a request against.
		if value == ">" || value == "|" || value == ">-" || value == "|-" {
			var block []string
			for j := i + 1; j < len(lines); j++ {
				next := lines[j]
				if strings.TrimSpace(next) == "" {
					block = append(block, "")
					continue
				}
				if indentOf(next) <= indent {
					break // dedented: the block ended
				}
				block = append(block, strings.TrimSpace(next))
				i = j
			}
			if value == ">" || value == ">-" {
				// Folded: newlines become spaces, blank lines become breaks.
				value = strings.TrimSpace(strings.Join(block, " "))
				value = strings.Join(strings.Fields(value), " ")
			} else {
				value = strings.TrimSpace(strings.Join(block, "\n"))
			}
			f.Value = value
		} else {
			f.Value = strings.Trim(value, `"'`)
			if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
				f.Kind = List
				f.List = splitInline(value[1 : len(value)-1])
			}
		}
		out = append(out, f)
		n := len(out) - 1
		if nested {
			out[open].Kind = Map
			out[open].List = nil
			owner = -1
			continue
		}
		open, owner = n, -1
		if value == "" {
			owner = n
		}
	}
	return out
}

// splitInline splits the inside of [a, "b", c] into unquoted items.
func splitInline(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, unquote(strings.TrimSpace(p)))
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
