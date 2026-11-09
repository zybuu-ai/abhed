package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Every spelling that could reach a resource a rule names, under another
// form, is refused or is the one form: a deny on docs://private/* holds.
func TestResourceURIsAreJudgedInOneForm(t *testing.T) {
	for in, want := range map[string]string{
		"docs://private/a":           "docs://private/a",
		"DOCS://private/a":           "docs://private/a",
		"docs://PRIVATE/a":           "docs://private/a",
		"docs://public/../private/a": "docs://public/private/a",
		"docs://private/./a":         "docs://private/a",
		"docs://private//a":          "docs://private/a",
		"docs://private/%61":         "docs://private/a",
		"docs://private/%2e%2e/x":    "docs://private/x",
		"docs://private/a%2fb":       "",
		"docs://private%2Fa":         "",
		"docs://private%5Ca":         "",
		`docs://private\a`:           "",
		"docs:/private/a":            "",
		"docs://user@private/a":      "",
		"docs://private/a#frag":      "",
		"docs://private/a b":         "",
		"docs://private/%zz":         "",
		"file:///my%20notes.md":      "file:///my%20notes.md",
		"file:///my%20notes%c3%a9":   "file:///my%20notes%C3%A9",
		"ticket:42":                  "ticket:42",
		"urn:isbn:0451450523":        "urn:isbn:0451450523",
		"docs://private/a?q=%2Fx":    "",
		"docs://private/dir/":        "docs://private/dir/",
	} {
		got, err := CanonicalResourceURI(in)
		if want == "" {
			if err == nil {
				t.Errorf("%s: accepted as %s", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, err, want)
		}
		if valid := validResourceURI(in); valid != (in == want) {
			t.Errorf("%s: read as given %v", in, valid)
		}
	}

	// Through the tool and policy: each spelling of a private resource is
	// denied or refused before the server is asked.
	s := newStubServer(t, "", true)
	g := NewGateway()
	if errs := connect(t, g, ServerConfig{Name: "probe", URL: s.srv.URL, Enabled: true}); len(errs) > 0 {
		t.Fatal(errs)
	}
	var read tools.Tool
	for _, tl := range g.Tools() {
		if tl.Name() == "mcp__probe__read_resource" {
			read = tl
		}
	}
	pol := policy.New(policy.ModeAuto)
	if err := pol.AddDeny("mcp__probe__read_resource(docs://private/*)"); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{"docs://private/a", "DOCS://private/a", "docs://PRIVATE/a", "docs://public/../private/a",
		"docs://private%2Fa", "docs:/private/a", "docs://private//a", "docs://private/%61"} {
		args, _ := json.Marshal(map[string]string{"uri": spelling})
		if pol.Evaluate(read.Name(), true, args).Decision == policy.Deny {
			continue
		}
		if res := read.Run(context.Background(), nil, args); !res.IsError {
			t.Errorf("%s: read past the deny", spelling)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reads {
		if strings.Contains(strings.ToLower(r), "private") {
			t.Fatalf("the server was asked for %s", r)
		}
	}
}
