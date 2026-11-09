package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
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
	if _, err := toolsOnly(reg, []string{"read", "recall"}); err != nil {
		t.Fatalf("the harness's own tool refused: %v", err)
	}

	cfg := config.Default()
	cfg.Extensions = []config.ExtensionConfig{{Name: "mine", Command: "/bin/true"}}
	cfg.Memory.Auto = true
	bare := bareConfig(cfg)
	if len(bare.Extensions) != 0 || bare.Memory.Auto {
		t.Fatalf("bare: %+v %v", bare.Extensions, bare.Memory.Auto)
	}
	if !bare.Skills.Disabled || !bare.Bare {
		t.Fatal("your own skills kept under -bare")
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

// Under -bare the organisation's rule and skill directories stay, and its
// rules still reach the prompt; the workspace's memory does not.
func TestBareKeepsManagedRulesAndSkills(t *testing.T) {
	rules, skills := t.TempDir(), t.TempDir()
	write(t, filepath.Join(rules, "org.md"), "ORG-RULE-CANARY")
	managedConfig(t, `{"rules":{"dirs":["`+rules+`"]},"skills":{"dirs":["`+skills+`"]}}`)
	ws := t.TempDir()
	write(t, filepath.Join(ws, "ABHED.md"), "WORKSPACE-CANARY")
	loaded, err := config.Load(ws)
	if err != nil {
		t.Fatal(err)
	}
	bare := bareConfig(loaded)
	if bare.Skills.Disabled || len(bare.Skills.Dirs) != 1 || len(bare.Rules.Dirs) != 1 {
		t.Fatalf("managed dirs under -bare: skills %+v rules %v", bare.Skills, bare.Rules.Dirs)
	}
	text := agent.LoadMemory(memoryOptions(bare, nil, ws)).Render()
	if !strings.Contains(text, "ORG-RULE-CANARY") || strings.Contains(text, "WORKSPACE-CANARY") {
		t.Fatalf("memory under -bare:\n%s", text)
	}
}

// init says it trusted only what it wrote when a local layer was there.
func TestInitTrustNote(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".abhed")
	path := filepath.Join(dir, "config.json")
	if note := initTrustNote(path); !strings.Contains(note, "trusts it as written") {
		t.Fatalf("without a local layer: %q", note)
	}
	write(t, filepath.Join(dir, config.LocalConfigName), "{}")
	note := initTrustNote(path)
	if strings.Contains(note, "trusts it as written") || !strings.Contains(note, config.LocalConfigName) || !strings.Contains(note, "asks") {
		t.Fatalf("with a local layer: %q", note)
	}
}
