package tools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestDecodeArgsRefusesWhatTwoReadersCouldReadTwoWays(t *testing.T) {
	for _, raw := range []string{
		`{"command":"a","command":"b"}`,
		`{"command":"a","Command":"b"}`,
		`{"command":"a","command":"b"}`,
		`{"hoſt":"a","host":"b"}`,
		`{"Kind":"a","kind":"b"}`,
		`{"x":{"path":"a","PATH":"b"}}`,
		`{"x":[{"path":"a"},{"path":"a","path":"b"}]}`,
		`{"command":"a"}{"command":"b"}`,
		`{"command":"a"} x`,
		`["command"]`,
		`"command"`,
		`{"command":"a"`,
	} {
		if _, err := DecodeArgs(json.RawMessage(raw)); !errors.Is(err, ErrMalformedArgs) {
			t.Errorf("%s: accepted", raw)
		}
	}
	for _, raw := range []string{``, ` `, `null`, `{}`, `{"a":{"a":1},"b":[{"a":1},{"a":2}]}`} {
		if _, err := DecodeArgs(json.RawMessage(raw)); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

// FoldKey must agree with encoding/json, which is how every struct reads a key.
func TestFoldKeyAgreesWithEncodingJSON(t *testing.T) {
	var v struct {
		Host string `json:"host"`
		Kind string `json:"kind"`
	}
	for _, k := range []string{"HOST", "hoſt", "Host", "Kind", "KIND"} {
		v.Host, v.Kind = "", ""
		_ = json.Unmarshal([]byte(`{"`+k+`":"x"}`), &v)
		matched := v.Host == "x" || v.Kind == "x"
		folds := FoldKey(k) == FoldKey("host") || FoldKey(k) == FoldKey("kind")
		if matched != folds {
			t.Errorf("%q: encoding/json matched %v, FoldKey %v", k, matched, folds)
		}
	}
	if FoldKey("hoſt") == FoldKey("hosts") {
		t.Fatal("fold is too loose")
	}
}

func TestCanonicalArgsForFixedAndOpenTools(t *testing.T) {
	for _, c := range []struct {
		tool Tool
		raw  string
		want string // empty means refused
	}{
		{Bash{}, `{"description":"d","command":"a<b"}`, `{"command":"a<b","description":"d"}`},
		{Bash{}, `{"command":"a","description":"d","path":"/x"}`, ""},
		{Bash{}, `{"Command":"a","description":"d"}`, ""},
		{Write{}, `{"path":"/a","content":"x","command":"ls"}`, ""},
		{Grep{}, `{"pattern":"x","context":12345678901234567890}`, `{"context":12345678901234567890,"pattern":"x"}`},
		{openTool{`{"properties":{"path":{}}}`}, `{"path":"/a","other":1}`, `{"other":1,"path":"/a"}`},
		{openTool{`{"properties":{"path":{}}}`}, `{"path":"/a","Path":"/b"}`, ""},
		{openTool{`{"properties":{"path":{}}}`}, `{"PATH":"/b"}`, ""},
		{openTool{`{"properties":{"path":{}}}`}, `{"path":"/a","command":"rm"}`, ""},
		{openTool{`{"properties":{"path":{}},"additionalProperties":false}`}, `{"path":"/a","other":1}`, ""},
		{openTool{`{}`}, `{"command":"x","anything":1}`, `{"anything":1,"command":"x"}`},
	} {
		got, err := CanonicalArgs(c.tool, json.RawMessage(c.raw))
		switch {
		case c.want == "" && err == nil:
			t.Errorf("%s %s: accepted as %s", c.tool.Name(), c.raw, got)
		case c.want != "" && (err != nil || string(got) != c.want):
			t.Errorf("%s %s: got %s, %v; want %s", c.tool.Name(), c.raw, got, err, c.want)
		}
	}
}

type openTool struct{ schema string }

func (openTool) Name() string                                          { return "open" }
func (openTool) Description() string                                   { return "" }
func (o openTool) Schema() json.RawMessage                             { return json.RawMessage(o.schema) }
func (openTool) Mutates() bool                                         { return true }
func (openTool) Run(context.Context, *Session, json.RawMessage) Result { return Result{} }

// A fixed tool's schema and its argument struct name the same keys.
func TestFixedToolSchemasMatchTheirStructs(t *testing.T) {
	for _, c := range []struct {
		tool Tool
		args any
	}{
		{Bash{}, bashArgs{}}, {Read{}, readArgs{}}, {Write{}, writeArgs{}}, {Edit{}, editArgs{}},
		{Glob{}, globArgs{}}, {Grep{}, grepArgs{}}, {Todo{}, todoArgs{}},
	} {
		if _, fixed := c.tool.(FixedArgs); !fixed {
			t.Errorf("%s is not marked fixed", c.tool.Name())
		}
		props, _ := schemaProps(c.tool.Schema())
		var fromSchema, fromStruct []string
		for p := range props {
			fromSchema = append(fromSchema, p)
		}
		rt := reflect.TypeOf(c.args)
		for i := 0; i < rt.NumField(); i++ {
			fromStruct = append(fromStruct, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
		}
		sort.Strings(fromSchema)
		sort.Strings(fromStruct)
		if !reflect.DeepEqual(fromSchema, fromStruct) {
			t.Errorf("%s: schema %v, struct %v", c.tool.Name(), fromSchema, fromStruct)
		}
	}
}
