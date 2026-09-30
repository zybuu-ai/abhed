package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

func parse(t *testing.T, args ...string) *cliFlags {
	t.Helper()
	var f cliFlags
	fs := newFlagSet(&f)
	fs.SetOutput(new(strings.Builder))
	if err := parseArgs(fs, &f, args); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return &f
}

func TestParseArgs(t *testing.T) {
	for _, c := range []struct {
		args   []string
		print  bool
		task   string
		sub    []string
		dashed bool
		format string
	}{
		{args: []string{"-p", "fix it", "-output-format", "json"}, print: true, task: "fix it", format: "json"},
		{args: []string{"-C", "/x", "-p", "hi"}, print: true, task: "hi"},
		{args: []string{"-p=inline task"}, print: true, task: "inline task"},
		{args: []string{"--print", "-output-format", "stream-json"}, print: true, format: "stream-json"},
		{args: []string{"fix the tests"}, task: "fix the tests"},
		{args: []string{"--", "fix", "-p", "it"}, task: "fix -p it", dashed: true},
		{args: []string{"serve", "-addr", ":9"}, sub: []string{"serve", "-addr", ":9"}},
		{args: []string{"-trust-workspace", "doctor"}, sub: []string{"doctor"}},
		// After -p a subcommand's name is only a word of the task.
		{args: []string{"-p", "doctor"}, print: true, task: "doctor"},
	} {
		f := parse(t, c.args...)
		if c.format == "" {
			c.format = "text"
		}
		if f.print.on != c.print || f.task() != c.task || !slices.Equal(f.sub, c.sub) || f.dashed != c.dashed || f.format != c.format {
			t.Errorf("%q: print %v task %q sub %q dashed %v format %s", c.args, f.print.on, f.task(), f.sub, f.dashed, f.format)
		}
	}
}

func TestPermissionFlag(t *testing.T) {
	for in, want := range map[string]string{"acceptEdits": "accept-edits", "bypassPermissions": "bypass", "plan": "plan"} {
		if got, err := parse(t, "-permission-mode", in).permission(); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, args := range [][]string{
		{"-permission-mode", "dontAsk"},
		{"-permission-mode", "plan", "-mode", "auto"},
		{"-dangerously-skip-permissions", "-mode", "plan"},
	} {
		if _, err := parse(t, args...).permission(); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	// The bypass switch sets nothing until it is confirmed.
	if m, err := parse(t, "-dangerously-skip-permissions").permission(); err != nil || m != "" {
		t.Errorf("bypass switch: %q %v", m, err)
	}
}

func TestToolRules(t *testing.T) {
	got := toolRules("Read, Edit,Bash(npm test:*),WebFetch,bash(go test*),Bash(echo a,b)")
	want := []string{"read", "edit", "bash(npm test*)", "web_fetch", "bash(go test*)", "bash(echo a,b)"}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	if j := joinRules("bash(ls*)", "Grep"); j != "bash(ls*),grep" {
		t.Fatalf("%q", j)
	}
}

func TestBudgetFlagOnlyLowersAManagedBudget(t *testing.T) {
	cfg := config.Config{ManagedKeys: []string{"limits.max_budget_tokens"}}
	cfg.Limits.MaxBudgetTokens = 1000
	if _, err := budgetFlag(cfg, 2000); err == nil {
		t.Fatal("raised a managed budget")
	}
	if c, err := budgetFlag(cfg, 500); err != nil || c.Limits.MaxBudgetTokens != 500 {
		t.Fatalf("%v %d", err, c.Limits.MaxBudgetTokens)
	}
}

func TestUserMessageText(t *testing.T) {
	for line, want := range map[string]string{
		`{"type":"user","message":{"role":"user","content":"hi"}}`:                                      "hi",
		`{"type":"user","message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}`: "a\nb",
		`{"type":"user","content":"flat"}`:                                                              "flat",
	} {
		if got, err := userMessageText([]byte(line)); err != nil || got != want {
			t.Errorf("%s: %q %v", line, got, err)
		}
	}
	for _, bad := range []string{`nope`, `{"type":"assistant","content":"x"}`, `{"type":"user","content":""}`} {
		if _, err := userMessageText([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestHeadlessTask(t *testing.T) {
	ctx := context.Background()
	var warn strings.Builder
	if got, _ := headlessTask(ctx, "summarise", strings.NewReader("the log\n"), true, &warn); got != "summarise\n\nInput from stdin:\nthe log" {
		t.Errorf("%q", got)
	}
	if got, _ := headlessTask(ctx, "", strings.NewReader("do this"), true, &warn); got != "do this" {
		t.Errorf("%q", got)
	}
	if got, _ := headlessTask(ctx, "only", strings.NewReader(""), true, &warn); got != "only" {
		t.Errorf("%q", got)
	}
	if _, err := headlessTask(ctx, "x", strings.NewReader(strings.Repeat("a", maxStdin+1)), true, &warn); err == nil {
		t.Error("oversized stdin accepted")
	}
}

func TestSystemPromptFlags(t *testing.T) {
	f := &cliFlags{appendSystem: "Be terse.", systemPrompt: "Replaced."}
	sp, err := systemPromptFlags(f, config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := sp.apply("base"); got != "Replaced.\n\nBe terse." {
		t.Fatalf("%q", got)
	}
	rec := map[string]any{}
	sp.record(rec)
	if rec["system_prompt"] != "replaced" || len(rec["system_prompt_sha256"].(string)) != 64 || rec["system_prompt_appended_sha256"] == nil {
		t.Fatalf("%v", rec)
	}
	for _, v := range rec {
		if s, ok := v.(string); ok && strings.Contains(s, "Replaced") {
			t.Fatal("the record holds the prompt's text")
		}
	}
	if _, err := systemPromptFlags(f, config.Config{Managed: true}); err == nil {
		t.Fatal("replaced under a managed configuration")
	}
	if _, err := systemPromptFlags(&cliFlags{appendSystem: "x"}, config.Config{Managed: true}); err != nil {
		t.Fatalf("appending under managed: %v", err)
	}
}

// After the yes, bypass is the session's mode, and session.started says so.
func TestSkipPermissionsTakesEffectAfterYes(t *testing.T) {
	cfg := config.Default()
	f := &cliFlags{skipPerms: true}
	if err := skipPermissions(cfg, f, func(config.Config) error { return errors.New("no") }); err == nil || f.mode != "" {
		t.Fatalf("a refusal set %q", f.mode)
	}
	if err := skipPermissions(cfg, f, func(config.Config) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := applyFlags(cfg, f.mode, 0, "", "", "")
	if err != nil || got.Permissions.Mode != "bypass" {
		t.Fatalf("mode %q %v", got.Permissions.Mode, err)
	}
	start := startPayload(got, f, true, "m")
	if start["mode"] != "bypass" || start["bypass_confirmed"] != true {
		t.Fatalf("%v", start)
	}
	if s := startPayload(config.Default(), &cliFlags{}, false, "m"); s["mode"] != "default" || s["bypass_confirmed"] != false {
		t.Fatalf("%v", s)
	}
}

// A comma inside a rule's parentheses stays in the rule, through the join
// and the split, in either spelling.
func TestRulesKeepCommasInParentheses(t *testing.T) {
	got, err := applyFlags(config.Default(), "", 0, joinRules("bash(echo x,y)", "Bash(git push origin a,b),Read"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bash(echo x,y)", "bash(git push origin a,b)", "read"} {
		if !slices.Contains(got.Permissions.Allow, want) {
			t.Errorf("no %q in %q", want, got.Permissions.Allow)
		}
	}
}
