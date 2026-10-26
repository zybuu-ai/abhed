package toolchain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// session is a toolchain session on a fake source with Go 1.21 and 1.22.
func session(t *testing.T) (*Session, *fake) {
	f := newFake(t)
	f.goRelease("1.21.13", true, goArchive(t, "1.21.13"), linuxAMD64)
	f.goRelease("1.22.5", true, goArchive(t, "1.22.5"), linuxAMD64)
	return &Session{Cache: testCache(t), Source: f.source(), Workspace: t.TempDir(),
		Platform: func() Platform { return linuxAMD64 }}, f
}

// Commands get the newest installed toolchain, or the one the project pins,
// first on PATH; a missing program is told how to install its language.
func TestSessionPreludeAndNotFound(t *testing.T) {
	s, _ := session(t)
	ctx := context.Background()
	if s.Prelude(s.Workspace) != "" {
		t.Fatal("a prelude with nothing installed")
	}
	for _, v := range []string{"1.21", "1.22"} {
		plan, err := s.Plan(ctx, Spec{Lang: "go", Version: v})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Install(ctx, plan, "user", "cli", "cli", nil); err != nil {
			t.Fatal(err)
		}
	}
	pre := s.Prelude(s.Workspace)
	if !strings.Contains(pre, filepath.Join(s.Cache.Root, "linux-amd64", "go", "1.22.5", "bin")) || !strings.HasSuffix(pre, `"${PATH:+:$PATH}"; `) {
		t.Fatalf("newest: %q", pre)
	}
	sub := filepath.Join(s.Workspace, "cmd", "x")
	_ = os.MkdirAll(sub, 0o755)
	_ = os.WriteFile(filepath.Join(s.Workspace, "go.mod"), []byte("module x\n\ngo 1.21\n"), 0o644)
	if pre := s.Prelude(sub); !strings.Contains(pre, filepath.Join("go", "1.21.13", "bin")) {
		t.Fatalf("a pin found from a subfolder: %q", pre)
	}
	if note := s.NotFound(sub, "go"); !strings.Contains(note, `name "go@1.21"`) || !strings.Contains(note, "pinned by go.mod") {
		t.Fatalf("note %q", note)
	}
	if s.NotFound(sub, "ls") != "" {
		t.Fatal("a note for a program no language provides")
	}
	s.Off = true
	if s.Prelude(sub) != "" || !strings.Contains(s.NotFound(sub, "go"), "turned off") {
		t.Fatal("off")
	}
	if _, err := s.Plan(ctx, Spec{Lang: "go"}); err != ErrOff {
		t.Fatalf("plan when off: %v", err)
	}
}

// Quoting holds a path with a quote in it.
func TestShellQuote(t *testing.T) {
	if got := shellQuote("/a b/it's"); got != `'/a b/it'\''s'` {
		t.Fatal(got)
	}
}

// The tool's call is put in the form an approval judges: name is the
// language and major, resolved what will be installed whatever the model
// wrote; the install runs only if that still holds, and records itself.
func TestToolResolvesInstallsAndRecords(t *testing.T) {
	s, f := session(t)
	tool := Tool{S: s}
	ts, err := tools.NewSession(s.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !tool.MutatesCall(json.RawMessage(`{"name":"go"}`)) || tool.MutatesCall(json.RawMessage(`{"list":true}`)) {
		t.Fatal("MutatesCall")
	}
	out, _, err := tool.ResolveArgs(ts, json.RawMessage(`{"name":"go@1.21","resolved":{"version":"9.9","url":"https://evil.example/x","digest":"sha256:00"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var a toolArgs
	_ = json.Unmarshal(out, &a)
	if a.Name != "go@1.21" || a.Resolved == nil || a.Resolved.Version != "1.21.13" || !strings.HasPrefix(a.Resolved.URL, f.srv.URL) ||
		a.Resolved.Why != "requested" {
		t.Fatalf("resolved %s", out)
	}
	if tg := tool.Target(ts, out); !strings.Contains(tg, "Go 1.21.13") || !strings.Contains(tg, a.Resolved.Digest) {
		t.Fatalf("target %q", tg)
	}
	res := tool.Run(context.Background(), ts, out)
	if res.IsError || !strings.Contains(res.Content, "Run the command again") || len(res.Events) != 1 {
		t.Fatalf("run %+v", res)
	}
	ev := res.Events[0].Payload.(agent.ToolchainAction)
	if res.Events[0].Type != string(agent.EvToolchainInstall) || ev.Outcome != agent.ToolchainOK || ev.Version != "1.21.13" || ev.By != "agent" {
		t.Fatalf("event %+v", res.Events[0])
	}
	// An approval for one build never installs another.
	stale := strings.Replace(string(out), "1.21.13", "1.21.12", 1)
	if res := tool.Run(context.Background(), ts, json.RawMessage(stale)); !res.IsError || !strings.Contains(res.Content, "changed since it was approved") {
		t.Fatalf("stale %+v", res)
	}
	if res := tool.Run(context.Background(), ts, json.RawMessage(`{"list":true}`)); res.IsError || !strings.Contains(res.Content, "go 1.21.13 (on PATH)") {
		t.Fatalf("list %+v", res)
	}
	if _, _, err := tool.ResolveArgs(ts, json.RawMessage(`{"name":"cobol"}`)); err == nil {
		t.Fatal("an unknown language resolved")
	}
}

// Agreeing to a toolchain opens its language's registries where the
// session allows it, recorded, and the approval names them; where it does
// not, the model is told why.
func TestToolOpensRegistries(t *testing.T) {
	s, _ := session(t)
	var opened []string
	s.Registries = func(lang string) ([]string, string) { return Registries[lang], "" }
	s.Open = func(lang string) ([]string, error) { opened = Registries[lang]; return opened, nil }
	tool := Tool{S: s}
	ts, _ := tools.NewSession(s.Workspace)
	out, _, err := tool.ResolveArgs(ts, json.RawMessage(`{"name":"go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if tg := tool.Target(ts, out); !strings.Contains(tg, "proxy.golang.org, sum.golang.org") {
		t.Fatalf("target %q", tg)
	}
	res := tool.Run(context.Background(), ts, out)
	if res.IsError || len(opened) != 2 || len(res.Events) != 2 || res.Events[1].Type != string(agent.EvToolchainRegistry) ||
		!strings.Contains(res.Content, "Commands can reach its package registries") {
		t.Fatalf("%+v", res)
	}
	s.Registries = func(string) ([]string, string) { return nil, "sandbox.toolchains_registries is off" }
	res = tool.Run(context.Background(), ts, out)
	if !strings.Contains(res.Content, "not open to commands: sandbox.toolchains_registries is off") || len(res.Events) != 1 {
		t.Fatalf("%+v", res)
	}
}
