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
		`{"command":"a","\u0063ommand":"b"}`,
		`{"command":"a","\u0043ommand":"b"}`,
		`{"ho\u017Ft":"a","host":"b"}`,
		`{"hoſt":"a","host":"b"}`,
		`{"\u212Aind":"a","kind":"b"}`,
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
	for _, k := range []string{"HOST", "hoſt", "Host", "\u212Aind", "KIND", "k\u0131nd"} {
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
		drop string
	}{
		{Bash{}, `{"description":"d","command":"a<b"}`, `{"command":"a<b","description":"d"}`, ""},
		{Bash{}, `{"command":"a","description":"d","path":"/x"}`, "", ""},
		{Bash{}, `{"Command":"a","description":"d"}`, "", ""},
		{Write{}, `{"path":"/a","content":"x","command":"ls"}`, "", ""},
		{Grep{}, `{"pattern":"x","context":12345678901234567890}`, `{"context":12345678901234567890,"pattern":"x"}`, ""},
		{Bash{}, `{"command":"ls","description":"d","timeout":5,"file_path":"x"}`, `{"command":"ls","description":"d"}`, "file_path,timeout"},
		{Read{}, `{"file_path":"/etc/x"}`, `{}`, "file_path"},
		{Read{}, `{"path":"/a","paths":["/b"]}`, `{"path":"/a"}`, "paths"},
		{openTool{`{"properties":{"path":{}}}`}, `{"path":"/a","other":1}`, `{"other":1,"path":"/a"}`, ""},
		{openTool{`{"properties":{"path":{}}}`}, `{"path":"/a","Path":"/b"}`, "", ""},
		{openTool{`{"properties":{"path":{}}}`}, `{"PATH":"/b"}`, "", ""},
		{openTool{`{"properties":{"path":{}}}`}, `{"path":"/a","command":"rm"}`, "", ""},
		{openTool{`{"properties":{"path":{}},"additionalProperties":false}`}, `{"path":"/a","other":1}`, "", ""},
		{openTool{`{}`}, `{"command":"x","anything":1}`, `{"anything":1,"command":"x"}`, ""},
	} {
		got, dropped, err := CanonicalArgs(c.tool, json.RawMessage(c.raw))
		switch {
		case c.want == "" && err == nil:
			t.Errorf("%s %s: accepted as %s", c.tool.Name(), c.raw, got)
		case c.want != "" && (err != nil || string(got) != c.want || strings.Join(dropped, ",") != c.drop):
			t.Errorf("%s %s: got %s dropping %v, %v; want %s dropping %s", c.tool.Name(), c.raw, got, dropped, err, c.want, c.drop)
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

func TestDepthLimitAndClippedKeys(t *testing.T) {
	deep := func(n int) string { return strings.Repeat(`{"a":`, n) + "1" + strings.Repeat("}", n) }
	if _, err := DecodeArgs(json.RawMessage(deep(maxArgsDepth))); err != nil {
		t.Fatalf("depth %d: %v", maxArgsDepth, err)
	}
	if _, err := DecodeArgs(json.RawMessage(deep(maxArgsDepth + 1))); !errors.Is(err, ErrMalformedArgs) {
		t.Fatalf("depth %d accepted", maxArgsDepth+1)
	}
	long := strings.Repeat("k", 1<<20)
	_, err := DecodeArgs(json.RawMessage(`{"` + long + `":1,"` + long + `":2}`))
	if err == nil || len(err.Error()) > 300 {
		t.Fatalf("a long key is echoed in full: %d bytes", len(err.Error()))
	}
	_, _, err = CanonicalArgs(Bash{}, json.RawMessage(`{"command":"a","`+strings.ToUpper(long[:100])+`":1,"COMMAND`+long[:10]+`":1}`))
	if err != nil && len(err.Error()) > 400 {
		t.Fatalf("a long key is echoed in full: %d bytes", len(err.Error()))
	}
}
