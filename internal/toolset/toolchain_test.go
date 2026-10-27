package toolset

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/toolchain"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// An install asks with the language and major version as its scope, so one
// "always allow" covers that language and major; auto and preapproved add
// allow rules, and off or a managed hold on the permissions adds none.
func TestToolchainPolicy(t *testing.T) {
	pol := policy.New(policy.ModeDefault)
	args := json.RawMessage(`{"name":"go@1.22","resolved":{"version":"1.22.5"}}`)
	d := pol.Evaluate(toolchain.ToolName, true, args)
	if d.Decision != policy.Ask || d.Offer() != "toolchain(go@1.22)" {
		t.Fatalf("%+v", d)
	}
	for _, c := range []struct {
		cfg  config.Config
		want string
	}{
		{config.Config{Sandbox: config.SandboxConfig{Toolchains: "auto"}}, "toolchain"},
		{config.Config{Sandbox: config.SandboxConfig{ToolchainsPreapproved: []string{"go", "python@3.12.4", "node@20"}}}, "toolchain(go@*) toolchain(python@3.12) toolchain(node@20)"},
		{config.Config{Sandbox: config.SandboxConfig{Toolchains: "off", ToolchainsPreapproved: []string{"go"}}}, ""},
		{config.Config{Managed: true, ManagedKeys: []string{"permissions.allow"}, Sandbox: config.SandboxConfig{Toolchains: "auto"}}, ""},
		{config.Config{Managed: true, ManagedKeys: []string{"permissions.allow", "sandbox.toolchains"}, Sandbox: config.SandboxConfig{Toolchains: "auto"}}, "toolchain"},
		// Each key widens a locked allow list only when the managed file sets that key.
		{config.Config{Managed: true, ManagedKeys: []string{"permissions.allow", "sandbox.toolchains_preapproved"},
			Sandbox: config.SandboxConfig{Toolchains: "auto", ToolchainsPreapproved: []string{"go"}}}, "toolchain(go@*)"},
		{config.Config{Managed: true, ManagedKeys: []string{"permissions.allow", "sandbox.toolchains"},
			Sandbox: config.SandboxConfig{Toolchains: "ask", ToolchainsPreapproved: []string{"go"}}}, ""},
	} {
		if got := strings.Join(ToolchainAllows(c.cfg), " "); got != c.want {
			t.Errorf("%+v: %q, want %q", c.cfg.Sandbox, got, c.want)
		}
	}
	pre := policy.New(policy.ModeDefault)
	_ = pre.AddAllow(ToolchainAllows(config.Config{Sandbox: config.SandboxConfig{ToolchainsPreapproved: []string{"go"}}})...)
	if d := pre.Evaluate(toolchain.ToolName, true, args); d.Decision != policy.Allow {
		t.Fatalf("preapproved go: %+v", d)
	}
	if d := pre.Evaluate(toolchain.ToolName, true, json.RawMessage(`{"name":"node@20"}`)); d.Decision != policy.Ask {
		t.Fatalf("node, not preapproved: %+v", d)
	}
}

// Bash's commands start with the active toolchains on PATH, inside the
// sandbox; a git command the host runs for the sandbox is passed on as is.
func TestBashGetsTheToolchains(t *testing.T) {
	root := t.TempDir()
	tc := &toolchain.Session{Cache: toolchain.Cache{Root: root}, Platform: toolchain.Host}
	var lines []string
	b := withToolchains(tools.Bash{Sandbox: func(ctx context.Context, cwd, line string) *exec.Cmd {
		lines = append(lines, line)
		return exec.Command("true")
	}}, tc)
	if b.NotFound == nil {
		t.Fatal("no NotFound")
	}
	b.Sandbox(context.Background(), t.TempDir(), "go version")
	if lines[0] != "go version" {
		t.Fatalf("with nothing installed: %q", lines[0])
	}
	off := withToolchains(tools.Bash{Sandbox: b.Sandbox}, &toolchain.Session{Off: true})
	if off.NotFound == nil || !strings.Contains(off.NotFound("", "go"), "turned off") {
		t.Fatal("off says so")
	}
}

// An interactive terminal gets the active toolchains on PATH as commands
// do, and a missing program there says which language Abhed installs.
func TestTerminalGetsTheToolchains(t *testing.T) {
	ws := t.TempDir()
	root := t.TempDir()
	dir := filepath.Join(root, toolchain.Host().String(), "go", "1.22.5")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "bin", "go"), []byte("#!/bin/sh\necho fake-go-1.22.5\n"), 0o755)
	marker, _ := json.Marshal(toolchain.Installed{Lang: "go", Version: "1.22.5", Platform: toolchain.Host().String()})
	_ = os.WriteFile(filepath.Join(dir, ".abhed-toolchain.json"), marker, 0o644)
	p := sandbox.DefaultPolicy(ws)
	p.ReadOnlyPaths = []string{root}
	sb := sandbox.NewProcess(p)
	if ok, why := sb.Available(); !ok {
		t.Skip(why)
	}
	defer sandbox.Close(sb)
	tc := &toolchain.Session{Cache: toolchain.Cache{Root: root}, Platform: toolchain.Host}
	b := withToolchains(tools.Bash{Sandbox: sb.Command, Shell: sb.Shell}, tc)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := b.Shell(ctx, ws)
	cmd.Stdin = strings.NewReader("go version\nrustc --version; echo rc=$?\nexit\n")
	out, _ := cmd.CombinedOutput()
	got := string(out)
	if !strings.Contains(got, "fake-go-1.22.5") {
		t.Fatalf("the terminal did not find go:\n%s", got)
	}
	if !strings.Contains(got, "rc=127") {
		t.Fatalf("a missing program did not exit 127:\n%s", got)
	}
	// macOS's own bash, 3.2, has no command_not_found_handle; bash 4 and later do.
	if !strings.Contains(got, "bash-3.") && !strings.Contains(got, "abhed: Rust is not installed in this sandbox") {
		t.Fatalf("no note for a missing program, or not 127:\n%s", got)
	}
}

// A deny or ask rule on a language holds for that language with its
// debugger, in every mode; an allow for the language alone does not cover
// the debugger.
func TestToolchainRulesHoldWithTheDebugger(t *testing.T) {
	for _, lang := range []string{"go", "python"} {
		plain := json.RawMessage(`{"name":"` + lang + `@3.12"}`)
		dbg := json.RawMessage(`{"name":"` + lang + `+debugger@3.12","debugger":true}`)
		deny := policy.New(policy.ModeDefault)
		_ = deny.AddAllow(toolchain.ToolName)
		_ = deny.AddDeny(toolchain.ToolName + "(" + lang + "@*)")
		for _, a := range []json.RawMessage{plain, dbg} {
			if d := deny.Evaluate(toolchain.ToolName, true, a); d.Decision != policy.Deny {
				t.Errorf("%s: deny %s@* with auto: %s %+v", lang, lang, a, d)
			}
		}
		ask := policy.New(policy.ModeDefault)
		_ = ask.AddAllow(toolchain.ToolName)
		_ = ask.AddAsk(toolchain.ToolName + "(" + lang + "@*)")
		if d := ask.Evaluate(toolchain.ToolName, true, dbg); d.Decision != policy.Ask {
			t.Errorf("%s: ask rule with the debugger: %+v", lang, d)
		}
		allow := policy.New(policy.ModeDefault)
		_ = allow.AddAllow(toolchain.ToolName + "(" + lang + "@*)")
		if d := allow.Evaluate(toolchain.ToolName, true, plain); d.Decision != policy.Allow {
			t.Errorf("%s: allow %s@*: %+v", lang, lang, d)
		}
		if d := allow.Evaluate(toolchain.ToolName, true, dbg); d.Decision != policy.Ask {
			t.Errorf("%s: an allow for the language covered its debugger: %+v", lang, d)
		}
	}
}

// Registries open from the start are named for the record, and only then.
func TestStartupRegistries(t *testing.T) {
	if g := StartupRegistries(config.Config{Sandbox: config.SandboxConfig{ToolchainsRegistries: "allow"}}, "cli"); len(g) != 1 ||
		g[0].Consent != "allow" || len(g[0].Hosts) == 0 || g[0].Surface != "cli" {
		t.Fatalf("allow: %+v", g)
	}
	for _, cfg := range []config.Config{
		{Sandbox: config.SandboxConfig{ToolchainsRegistries: "ask"}},
		{Sandbox: config.SandboxConfig{ToolchainsRegistries: "off"}},
		{Sandbox: config.SandboxConfig{ToolchainsRegistries: "allow", AllowNetwork: true}},
	} {
		if g := StartupRegistries(cfg, "cli"); g != nil {
			t.Errorf("%+v: %+v", cfg.Sandbox, g)
		}
	}
}

// A deny or ask rule on a language holds for it with its language server,
// alone or with the debugger; an allow for the language covers neither.
func TestToolchainRulesHoldWithTheLanguageServer(t *testing.T) {
	for _, lang := range []string{"go", "python", "node", "rust"} {
		for _, name := range []string{lang + "+lsp@1.2", lang + "+debugger+lsp@1.2"} {
			call := json.RawMessage(`{"name":"` + name + `","lsp":true}`)
			deny := policy.New(policy.ModeDefault)
			_ = deny.AddAllow(toolchain.ToolName)
			_ = deny.AddDeny(toolchain.ToolName + "(" + lang + "@*)")
			if d := deny.Evaluate(toolchain.ToolName, true, call); d.Decision != policy.Deny {
				t.Errorf("%s: deny %s@* with auto: %+v", name, lang, d)
			}
			ask := policy.New(policy.ModeDefault)
			_ = ask.AddAllow(toolchain.ToolName)
			_ = ask.AddAsk(toolchain.ToolName + "(" + lang + "@*)")
			if d := ask.Evaluate(toolchain.ToolName, true, call); d.Decision != policy.Ask {
				t.Errorf("%s: ask rule: %+v", name, d)
			}
			allow := policy.New(policy.ModeDefault)
			_ = allow.AddAllow(toolchain.ToolName + "(" + lang + "@*)")
			if d := allow.Evaluate(toolchain.ToolName, true, call); d.Decision != policy.Ask {
				t.Errorf("%s: an allow for the language covered its server: %+v", name, d)
			}
		}
	}
}

// Deny and ask rules on a language see it whatever follows its "+", as the
// tool reads the language before the first "+".
func TestToolchainRulesHoldWithAnySuffix(t *testing.T) {
	deny := policy.New(policy.ModeDefault)
	_ = deny.AddAllow(toolchain.ToolName)
	_ = deny.AddDeny(toolchain.ToolName + "(go@*)")
	for _, name := range []string{"go+bogus+lsp@1.27", "go+other@1.27"} {
		if d := deny.Evaluate(toolchain.ToolName, true, json.RawMessage(`{"name":"`+name+`"}`)); d.Decision != policy.Deny {
			t.Errorf("%s: %+v", name, d)
		}
	}
}

// A command with an installed Go gets that Go's GOROOT, whatever the host's
// is, and the host's settings that would point a toolchain elsewhere go.
func TestToolchainOverridesTheHostsSettings(t *testing.T) {
	ws := t.TempDir()
	root := t.TempDir()
	dir := filepath.Join(root, toolchain.Host().String(), "go", "1.26.0")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "bin", "go"), []byte("#!/bin/sh\necho \"goroot=$GOROOT toolchain=$GOTOOLCHAIN pythonhome=${PYTHONHOME:-unset}\"\n"), 0o755)
	marker, _ := json.Marshal(toolchain.Installed{Lang: "go", Version: "1.26.0", Platform: toolchain.Host().String()})
	_ = os.WriteFile(filepath.Join(dir, ".abhed-toolchain.json"), marker, 0o644)
	pdir := filepath.Join(root, toolchain.Host().String(), "python", "3.12.7")
	_ = os.MkdirAll(filepath.Join(pdir, "bin"), 0o755)
	pmarker, _ := json.Marshal(toolchain.Installed{Lang: "python", Version: "3.12.7", Platform: toolchain.Host().String()})
	_ = os.WriteFile(filepath.Join(pdir, ".abhed-toolchain.json"), pmarker, 0o644)
	t.Setenv("GOROOT", "/usr/local/go")
	t.Setenv("GOTOOLCHAIN", "auto")
	t.Setenv("PYTHONHOME", "/opt/host-python")
	var line string
	b := withToolchains(tools.Bash{Sandbox: func(ctx context.Context, cwd, l string) *exec.Cmd {
		line = l
		return exec.CommandContext(ctx, "/bin/sh", "-c", l) // #nosec G204 -- the test's own line
	}}, &toolchain.Session{Cache: toolchain.Cache{Root: root}, Platform: toolchain.Host})
	out, err := b.Sandbox(context.Background(), ws, "go version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "goroot="+dir+" toolchain=local pythonhome=unset") {
		t.Fatalf("%v %s\n%s", err, out, line)
	}
}

// An opt-in check with a real Go in a toolchain cache: with another Go's
// GOROOT on the host, a build in the process sandbox uses the cache's.
// ABHED_TEST_TOOLCHAIN_CACHE names the cache; ABHED_TEST_HOST_GOROOT the other Go.
func TestRealGoBuildsWithAHostGOROOT(t *testing.T) {
	root, other := os.Getenv("ABHED_TEST_TOOLCHAIN_CACHE"), os.Getenv("ABHED_TEST_HOST_GOROOT")
	if root == "" || other == "" {
		t.Skip("set ABHED_TEST_TOOLCHAIN_CACHE and ABHED_TEST_HOST_GOROOT to build with a real Go")
	}
	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module hello\n\ngo 1.22\n"), 0o644)
	_ = os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nfunc main() { println(\"built\") }\n"), 0o644)
	t.Setenv("GOROOT", other)
	p := sandbox.DefaultPolicy(ws)
	p.ReadOnlyPaths = []string{root}
	sb := sandbox.NewProcess(p)
	if ok, why := sb.Available(); !ok {
		t.Skip(why)
	}
	defer sandbox.Close(sb)
	b := withToolchains(tools.Bash{Sandbox: sb.Command}, &toolchain.Session{Cache: toolchain.Cache{Root: root}, Platform: toolchain.Host})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := b.Sandbox(ctx, ws, "go env GOROOT && go run .").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "built") || strings.Contains(string(out), other) {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// An opt-in check with a real Rust in a toolchain cache: a crate builds and
// its tests run in the process sandbox. ABHED_TEST_TOOLCHAIN_CACHE names the cache.
func TestRealRustBuildsInTheSandbox(t *testing.T) {
	root := os.Getenv("ABHED_TEST_TOOLCHAIN_CACHE")
	if root == "" || len(toolchainsIn(root, "rust")) == 0 {
		t.Skip("set ABHED_TEST_TOOLCHAIN_CACHE to a cache with Rust to build with it")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := filepath.Join(home, "code", "rustcalc")
	_ = os.MkdirAll(filepath.Join(ws, "src"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, "Cargo.toml"), []byte("[package]\nname = \"rustcalc\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(ws, "src", "lib.rs"), []byte("pub fn f(n: u64) -> u64 { (1..=n).product() }\n#[cfg(test)]\nmod t { #[test] fn five() { assert_eq!(super::f(5), 120); } }\n"), 0o644)
	p := sandbox.DefaultPolicy(ws)
	p.ReadOnlyPaths = []string{root}
	sb := sandbox.NewProcess(p)
	if ok, why := sb.Available(); !ok {
		t.Skip(why)
	}
	defer sandbox.Close(sb)
	b := withToolchains(tools.Bash{Sandbox: sb.Command}, &toolchain.Session{Cache: toolchain.Cache{Root: root}, Platform: toolchain.Host})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := b.Sandbox(ctx, ws, "cargo test --offline 2>&1").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "test result: ok") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// toolchainsIn are the installed versions of lang for this host in root.
func toolchainsIn(root, lang string) []toolchain.Installed {
	return toolchain.Cache{Root: root}.List(toolchain.Host(), lang)
}
