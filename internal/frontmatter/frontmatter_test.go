package frontmatter

import (
	"errors"
	"reflect"
	"testing"
)

func field(t *testing.T, d *Document, key string) Field {
	t.Helper()
	for _, f := range d.Top() {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("no top-level key %q in %+v", key, d.Fields)
	return Field{}
}

// The three list spellings an agent file uses all read as the same list.
func TestListsInlineAndItems(t *testing.T) {
	d, err := Parse("---\nname: a\ninline: [Read, \"Grep\", 'glob']\nitems:\n  - read\n  - \"bash\"\nflat:\n- one\ncomma: Read, Grep\nempty: []\n---\nbody")
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string][]string{
		"inline": {"Read", "Grep", "glob"},
		"items":  {"read", "bash"},
		"flat":   {"one"},
		"empty":  {},
	} {
		f := field(t, d, key)
		if f.Kind != List || !reflect.DeepEqual(f.List, want) {
			t.Fatalf("%s: got %v %q, want list %q", key, f.Kind, f.List, want)
		}
	}
	if f := field(t, d, "comma"); f.Kind != Scalar || f.Value != "Read, Grep" {
		t.Fatalf("a comma string is a scalar its reader splits: %+v", f)
	}
	if d.Body != "body" {
		t.Fatalf("body: %q", d.Body)
	}
}

// A nested block is reported as nested under a Map key, never as top-level
// keys: a definition's hooks: block must not be read as settings of its own.
func TestNestedBlockIsNotTopLevel(t *testing.T) {
	d, err := Parse("---\nname: a\nhooks:\n  model: other\n  tools: bash\ndescription: d\n---\nb")
	if err != nil {
		t.Fatal(err)
	}
	if f := field(t, d, "hooks"); f.Kind != Map {
		t.Fatalf("hooks should be a map: %+v", f)
	}
	for _, f := range d.Top() {
		if f.Key == "model" || f.Key == "tools" {
			t.Fatalf("a nested key was read as top-level: %+v", f)
		}
	}
	if f := field(t, d, "description"); f.Value != "d" {
		t.Fatalf("the key after a nested block: %+v", f)
	}
	nested := 0
	for _, f := range d.Fields {
		if f.Nested {
			nested++
		}
	}
	if nested != 2 {
		t.Fatalf("want the 2 nested keys reported, got %d", nested)
	}
}

func TestBlockScalars(t *testing.T) {
	d, err := Parse("---\nfolded: >\n  one\n  two\nliteral: |\n  a\n  b\n---\nx")
	if err != nil {
		t.Fatal(err)
	}
	if v := field(t, d, "folded").Value; v != "one two" {
		t.Fatalf("folded: %q", v)
	}
	if v := field(t, d, "literal").Value; v != "a\nb" {
		t.Fatalf("literal: %q", v)
	}
}

func TestErrors(t *testing.T) {
	if _, err := Parse("no header"); !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
	if _, err := Parse("---\nname: a\n"); !errors.Is(err, ErrUnclosed) {
		t.Fatalf("want ErrUnclosed, got %v", err)
	}
}
