package app

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// -tools keeps the named tools and no others, and refuses a name the
// session lacks; -bare keeps only the organisation's extensions and turns
// auto memory off unless the managed file turns it on.
func TestToolsOnlyAndBare(t *testing.T) {
	reg := tools.NewRegistry(tools.Read{}, tools.Glob{}, tools.Bash{})
	sub, err := toolsOnly(reg, []string{"read", "glob"})
	if err != nil || strings.Join(sub.Names(), ",") != "read,glob" && strings.Join(sub.Names(), ",") != "glob,read" {
		t.Fatalf("subset: %v %v", sub, err)
	}
	if _, has := sub.Get("bash"); has {
		t.Fatal("bash kept")
	}
	if _, err := toolsOnly(reg, []string{"read", "nosuch"}); err == nil || !strings.Contains(err.Error(), "nosuch") {
		t.Fatalf("unknown: %v", err)
	}

	cfg := config.Default()
	cfg.Extensions = []config.ExtensionConfig{{Name: "mine", Command: "/bin/true"}}
	cfg.Memory.Auto = true
	bare := bareConfig(cfg)
	if len(bare.Extensions) != 0 || bare.Memory.Auto {
		t.Fatalf("bare: %+v %v", bare.Extensions, bare.Memory.Auto)
	}
	if runParts(true)&toolset.Skills != 0 || runParts(false)&toolset.Skills == 0 {
		t.Fatal("skills kept under -bare, or dropped without it")
	}

	// The organisation's extensions and auto memory stay.
	managedConfig(t, `{"extensions":[{"name":"audit","command":"/bin/true"}],"memory":{"auto":true}}`)
	loaded, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	loaded.Extensions = append(loaded.Extensions, config.ExtensionConfig{Name: "mine", Command: "/bin/true"})
	bare = bareConfig(loaded)
	if len(bare.Extensions) != 1 || bare.Extensions[0].Name != "audit" || !bare.Memory.Auto {
		t.Fatalf("managed under -bare: %+v %v", bare.Extensions, bare.Memory.Auto)
	}
}

// From the command line: -tools narrows what the run records it has, and a
// name the session lacks stops the run; -bare is recorded.
func TestToolsAndBareFlags(t *testing.T) {
	m := newScriptModel(t)
	ws := bgWorkspace(t, m.URL, "")
	run := func(args ...string) (string, string, int) {
		var out, errb strings.Builder
		cmd := mainHelper(append([]string{"-C", ws}, args...))
		cmd.Stdout, cmd.Stderr = &out, &errb
		code := 0
		if err := cmd.Run(); err != nil {
			code = cmd.ProcessState.ExitCode()
		}
		return out.String(), errb.String(), code
	}
	out, errOut, code := run("-p", "hi", "-tools", "read,glob", "-bare", "-output-format", "json")
	if code != 0 || !strings.Contains(out, `"tools_only":["read","glob"]`) && !strings.Contains(out, `"tools_only":["glob","read"]`) || !strings.Contains(out, `"bare":true`) {
		t.Fatalf("recorded: %d\n%s\n%s", code, out, errOut)
	}
	if _, errOut, code := run("-p", "hi", "-tools", "read,nosuch"); code != 2 || !strings.Contains(errOut, "-tools: this session has no tool nosuch") {
		t.Fatalf("unknown tool: %d %s", code, errOut)
	}
}
