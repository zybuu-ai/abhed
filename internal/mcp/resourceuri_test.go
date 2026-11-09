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
		"docs://private/a":                  "docs://private/a",
		"DOCS://private/a":                  "docs://private/a",
		"docs://PRIVATE/a":                  "docs://private/a",
		"docs://public/../private/a":        "docs://public/private/a",
		"docs://private/./a":                "docs://private/a",
		"docs://private//a":                 "docs://private/a",
		"docs://private/%61":                "docs://private/a",
		"docs://private/%2e%2e/x":           "docs://private/x",
		"docs://private/a%2fb":              "",
		"docs://private%2Fa":                "",
		"docs://private%5Ca":                "",
		`docs://private\a`:                  "",
		"docs:/private/a":                   "",
		"docs://user@private/a":             "",
		"docs://private/a#frag":             "",
		"docs://private/a b":                "",
		"docs://private/%zz":                "",
		"file:///my%20notes.md":             "file:///my%20notes.md",
		"file:///my%20notes%c3%a9":          "file:///my%20notesé",
		"ticket:42":                         "ticket:42",
		"urn:isbn:0451450523":               "urn:isbn:0451450523",
		"docs://private/a?q=%2Fx":           "",
		"docs://private/dir/":               "docs://private/dir/",
		"docs://private./a":                 "docs://private/a",
		"https://h:443/private/a":           "https://h/private/a",
		"http://h:80/private/a":             "http://h/private/a",
		"https://h:0443/a":                  "https://h/a",
		"https://h:8443/a":                  "https://h:8443/a",
		"docs://h:443/a":                    "docs://h:443/a",
		"file://localhost/reports/a":        "file:///reports/a",
		"file://LOCALHOST./reports/a":       "file:///reports/a",
		"docs://ｄｏｃｓ.example/a":             "",
		"docs://café.example/a":             "",
		"docs://xn--caf-dma.example/a":      "docs://xn--caf-dma.example/a",
		"docs://caf%C3%A9.example/a":        "",
		"docs://%64ocs.example/a":           "docs://docs.example/a",
		"docs://h:99999/a":                  "",
		"docs://h:x/a":                      "",
		"docs://[::1]:80/a":                 "docs://[::1]:80/a",
		"docs://[0:0::1]/a":                 "docs://[::1]/a",
		"docs://[::ffff:127.0.0.1]/a":       "",
		"docs://[nope]/a":                   "",
		"docs://127.1/a":                    "",
		"docs://0x7f.0.0.1/a":               "",
		"docs://127.000.0.1/a":              "",
		"docs://127.0.0.1/a":                "docs://127.0.0.1/a",
		"docs://private/caf%C3%A9/x":        "docs://private/café/x",
		"docs://private/caf%c3%a9/x":        "docs://private/café/x",
		"docs://private/cafe\u0301/x":       "docs://private/café/x",
		"docs://private/café/x":             "docs://private/café/x",
		"docs://h/pub%252F..%252Fprivate/a": "docs://h/pub%252F..%252Fprivate/a",
		"docs://h/100%25":                   "docs://h/100%25",
		"docs://h/pub／..／private/a":         "docs://h/pub／..／private/a",
		"docs://h/ﬁle":                      "docs://h/ﬁle",
		"docs://h/x?q=ｆ":                    "docs://h/x?q=ｆ",
		"docs://private/%C3/x":              "docs://private/%C3/x",
		"docs://private/%C2%A0/x":           "docs://private/%C2%A0/x",
		"docs://private/%E2%80%8B/x":        "docs://private/%E2%80%8B/x",
		"http://[::1]:80/a":                 "http://[::1]/a",
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
	// A server that folds case or normalises Unicode reads another spelling of
	// the path as the same resource: deny and ask rules match those too.
	for rule, spellings := range map[string][]string{
		"mcp__probe__read_resource(file:///reports/private/*)": {"file:///reports/PRIVATE/a", "file:///reports/Private/a"},
		"mcp__probe__read_resource(file:///reports/café/*)":    {"file:///reports/cafe\u0301/a", "file:///reports/CAFÉ/a", "file:///reports/caf%C3%A9/a", "file:///reports/caf%c3%a9/a"},
		"mcp__probe__read_resource(docs://private/café/*)":     {"docs://private/caf%C3%A9/x", "docs://private/café/x"},
		"mcp__probe__read_resource(docs://private/*)":          {"docs://private./a"},
		"mcp__probe__read_resource(https://h/private/*)":       {"https://h:443/private/a"},
		"mcp__probe__read_resource(file:///reports/*)":         {"file://localhost/reports/a"},
	} {
		p := policy.New(policy.ModeAuto)
		if err := p.AddDeny(rule); err != nil {
			t.Fatal(err)
		}
		for _, spelling := range spellings {
			args, _ := json.Marshal(map[string]string{"uri": spelling})
			if p.Evaluate(read.Name(), true, args).Decision == policy.Deny {
				continue
			}
			if strings.Contains(rule, "café") {
				t.Errorf("%s: %s not denied by the rule itself", rule, spelling)
			}
			if res := read.Run(context.Background(), nil, args); !res.IsError {
				t.Errorf("%s: %s read past it", rule, spelling)
			}
		}
		// Allow rules still match only as written.
		ap := policy.New(policy.ModeDefault)
		if err := ap.AddAllow("mcp__probe__read_resource(file:///reports/private/*)"); err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]string{"uri": "file:///reports/PRIVATE/a"})
		if ap.Evaluate(read.Name(), true, args).Decision == policy.Allow {
			t.Error("an allow rule matched another case")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reads {
		if low := strings.ToLower(r); strings.Contains(low, "private") || strings.Contains(low, "caf") || strings.Contains(low, "reports/a") {
			t.Fatalf("the server was asked for %s", r)
		}
	}
}

// Names a person writes with compatibility characters or a literal % are
// read as written; spellings a server folding NFKC or decoding twice would
// read as a denied resource are denied by the rule itself.
func TestCompatibilityAndDoubleEscapes(t *testing.T) {
	for _, uri := range []string{"docs://h/Notes…%20draft.md", "docs://h/TM™.md", "docs://h/10µs.md", "docs://h/ｶﾀｶﾅ.md",
		"docs://h/x².md", "docs://h/①.md", "docs://h/100%25AB.md", "docs://h/ﬁnance.md"} {
		if c, err := CanonicalResourceURI(uri); err != nil || c != uri {
			t.Errorf("%s is not readable as written: %q %v", uri, c, err)
		}
	}
	tools.RegisterSubjector("mcp__compat__read_resource", readSubject{})
	t.Cleanup(func() { tools.UnregisterSubjector("mcp__compat__read_resource") })
	pol := policy.New(policy.ModeBypass)
	if err := pol.AddDeny("mcp__compat__read_resource(docs://h/private/*)"); err != nil {
		t.Fatal(err)
	}
	if err := pol.AddDeny("mcp__compat__read_resource(docs://h/files/*)"); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"docs://h/pub／..／private/a", "docs://h/ｐrivate/a", "docs://h/ＰＲＩＶＡＴＥ/a",
		"docs://h/pub%252F..%252Fprivate/a", "docs://h/%252E%252E/private/a", "docs://h/ﬁles/a"} {
		args, _ := json.Marshal(map[string]string{"uri": uri})
		if d := pol.Evaluate("mcp__compat__read_resource", true, args); d.Decision != policy.Deny {
			t.Errorf("%s passed the deny: %v", uri, d.Decision)
		}
	}
	// And the readable ones are not denied by an unrelated rule.
	for _, uri := range []string{"docs://h/TM™.md", "docs://h/100%25AB.md", "docs://h/ｶﾀｶﾅ.md"} {
		args, _ := json.Marshal(map[string]string{"uri": uri})
		if d := pol.Evaluate("mcp__compat__read_resource", true, args); d.Decision == policy.Deny {
			t.Errorf("%s denied: %s", uri, d.Reason)
		}
	}
}
