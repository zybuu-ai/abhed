package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAgent(t *testing.T, ws, name, body string) string {
	t.Helper()
	dir := filepath.Join(ws, ".abhed", "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const reviewerDef = "---\nname: reviewer\ndescription: reviews\nmodel: other\ntools: read, grep\n---\nReview it."

// A workspace's definitions load only under a decision about their content:
// untrusted they are listed as ignored, trusted they load, and an edit to one
// makes them untrusted again with the reason "changed".
func TestWorkspaceAgentsTrustCoversContent(t *testing.T) {
	_, ws := trustHome(t, "", "")
	p := writeAgent(t, ws, "reviewer.md", reviewerDef)

	st, err := InspectWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	if st.AgentsTrusted || st.AgentsReason != "new" || len(st.Agents) != 1 || st.AgentsSHA256 == "" {
		t.Fatalf("new definitions: %+v", st)
	}
	if !st.NeedsDecision() {
		t.Fatal("a workspace with definitions and no config.json produced no trust decision")
	}
	if w := st.Warning(); !strings.Contains(w, "agents/reviewer") {
		t.Fatalf("the warning does not name the ignored definition: %q", w)
	}
	if f := st.AgentFiles(); len(f) != 1 || string(f[0].Data) != reviewerDef {
		t.Fatal("the files handed to the loader are not what was hashed")
	}

	if err := GrantReviewed(ws, st.Reviewed()); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Workspace.AgentsTrusted || cfg.Workspace.AgentsReason != "stored" || len(cfg.Workspace.AgentFiles()) != 1 {
		t.Fatalf("granted definitions: %+v", cfg.Workspace)
	}

	if err := os.WriteFile(p, []byte(strings.Replace(reviewerDef, "model: other", "model: elsewhere", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.AgentsTrusted || cfg.Workspace.AgentsReason != "changed" || !cfg.Workspace.NeedsDecision() {
		t.Fatalf("an edited definition is still trusted: %+v", cfg.Workspace)
	}

	// A new file changes the hash as much as an edit does.
	st, _ = InspectWorkspace(ws)
	if err := GrantReviewed(ws, st.Reviewed()); err != nil {
		t.Fatal(err)
	}
	writeAgent(t, ws, "second.md", reviewerDef)
	if st, _ = InspectWorkspace(ws); st.AgentsTrusted || st.AgentsReason != "changed" {
		t.Fatalf("an added definition is trusted: %+v", st)
	}
}

// Old trust records carry no agents hash. A workspace without definitions
// keeps its trusted file with no new prompt; one with definitions keeps its
// file trusted and is asked about the definitions alone.
func TestOldTrustRecordsDoNotRePrompt(t *testing.T) {
	home, ws := trustHome(t, "", `{"permissions":{"mode":"bypass"}}`)
	st, _ := InspectWorkspace(ws)
	old := map[string]any{"version": 1, "workspaces": map[string]any{
		st.Workspace: map[string]any{"sha256": st.SHA256, "decision": "trusted", "at": "2026-09-01T00:00:00Z"}}}
	data, _ := json.Marshal(old)
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "trust.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Workspace.Trusted || cfg.Workspace.NeedsDecision() || cfg.Permissions.Mode != "bypass" {
		t.Fatalf("an old record without definitions now re-prompts: %+v", cfg.Workspace)
	}

	writeAgent(t, ws, "reviewer.md", reviewerDef)
	cfg, err = LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	w := cfg.Workspace
	if !w.Trusted || w.AgentsTrusted || w.AgentsReason != "new" || !w.NeedsDecision() {
		t.Fatalf("definitions added under an old record: %+v", w)
	}
	// Declining the definitions keeps the file the person trusted.
	if err := RecordDecision(ws, w.Reviewed(), true, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ = LoadWith(ws, LoadOptions{Quiet: true})
	if w = cfg.Workspace; !w.Trusted || w.AgentsTrusted || w.AgentsReason != "declined" || w.NeedsDecision() {
		t.Fatalf("declining the definitions: %+v", w)
	}
}

// A definition that is a link, or has a second name, is refused and never
// hashed or handed to the loader; so is a linked agents directory.
func TestWorkspaceAgentLinksRefused(t *testing.T) {
	_, ws := trustHome(t, "", "")
	writeAgent(t, ws, "real.md", reviewerDef)
	secret := filepath.Join(ws, ".abhed", "users.json")
	if err := os.WriteFile(secret, []byte(`{"users":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(ws, ".abhed", "agents", "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(ws, ".abhed", "agents", "hard.md")); err != nil {
		t.Fatal(err)
	}
	st, err := InspectWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 1 || st.Agents[0] != ".abhed/agents/real.md" || len(st.AgentsProblems) != 2 {
		t.Fatalf("links were read as definitions: %+v", st)
	}
	if p := strings.Join(st.AgentsProblems, "\n"); !strings.Contains(p, "link.md: not a regular file") || !strings.Contains(p, "hard.md: has 2 names") {
		t.Fatalf("the refusals do not say why: %s", p)
	}

	other := t.TempDir()
	writeAgent(t, other, "x.md", reviewerDef)
	ws2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws2, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, ".abhed", "agents"), filepath.Join(ws2, ".abhed", "agents")); err != nil {
		t.Fatal(err)
	}
	if st, _ := InspectWorkspace(ws2); len(st.Agents) != 0 || len(st.AgentsProblems) != 1 {
		t.Fatalf("a linked agents directory was read: %+v", st)
	}
}

// The flag and the environment trust the definitions for one run; a refusal
// refuses them whatever was recorded; the home directory's are the user's own.
func TestAgentsTrustChoices(t *testing.T) {
	home, ws := trustHome(t, "", "")
	writeAgent(t, ws, "reviewer.md", reviewerDef)
	st, _ := InspectWorkspace(ws)
	if err := GrantReviewed(ws, st.Reviewed()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		opt    LoadOptions
		env    string
		trust  bool
		reason string
	}{
		{LoadOptions{Trust: TrustRefused}, "", false, "refused"},
		{LoadOptions{Trust: TrustGranted}, "", true, "flag"},
		{LoadOptions{}, "1", true, "env"},
	} {
		// The first case also shows a stored grant does not outvote a refusal.
		t.Setenv(TrustEnv, c.env)
		cfg, err := LoadWith(ws, LoadOptions{Trust: c.opt.Trust, Quiet: true})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Workspace.AgentsTrusted != c.trust || cfg.Workspace.AgentsReason != c.reason {
			t.Fatalf("%+v: got %v %q", c, cfg.Workspace.AgentsTrusted, cfg.Workspace.AgentsReason)
		}
	}
	t.Setenv(TrustEnv, "")
	writeAgent(t, home, "mine.md", reviewerDef)
	if st, _ := InspectWorkspace(home); !st.AgentsTrusted || st.AgentsReason != "home" {
		t.Fatalf("the home directory's definitions: %+v", st)
	}
}

// A decision made for definitions alone says nothing about a config.json
// added later: that file is new, not changed.
func TestAgentsOnlyRecordLeavesALaterFileNew(t *testing.T) {
	_, ws := trustHome(t, "", "")
	writeAgent(t, ws, "reviewer.md", reviewerDef)
	st, _ := InspectWorkspace(ws)
	if err := GrantReviewed(ws, st.Reviewed()); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, ws, `{"permissions":{"mode":"bypass"}}`)
	st, _ = InspectWorkspace(ws)
	if st.Trusted || st.Reason != "new" || !st.AgentsTrusted {
		t.Fatalf("a file added after a definitions-only grant: %+v", st)
	}
}

// An untrusted workspace file cannot add agent directories, and may turn
// definitions off.
func TestUntrustedAgentsKeys(t *testing.T) {
	_, ws := trustHome(t, "", `{"agents":{"dirs":["/tmp/theirs"]}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents.Dirs) != 0 || !ignored(cfg.Workspace, "agents.dirs") {
		t.Fatalf("an untrusted file added agent directories: %+v", cfg.Agents)
	}
	writeConfig(t, ws, `{"agents":{"disabled":true}}`)
	if cfg, _ = LoadWith(ws, LoadOptions{Quiet: true}); !cfg.Agents.Disabled {
		t.Fatal("an untrusted file could not turn definitions off")
	}
}

// A reload keeps trusted content trusted, and trusts changed content only by
// a stored decision for it.
func TestRefreshAgents(t *testing.T) {
	_, ws := trustHome(t, "", "")
	writeAgent(t, ws, "reviewer.md", reviewerDef)
	cfg, _ := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
	if st := RefreshAgents(cfg.Workspace); !st.AgentsTrusted || st.AgentsReason != "flag" || len(st.AgentFiles()) != 1 {
		t.Fatalf("unchanged content after a reload: %+v", st)
	}
	writeAgent(t, ws, "reviewer.md", reviewerDef+" Changed.")
	st := RefreshAgents(cfg.Workspace)
	if st.AgentsTrusted || st.AgentsReason != "new" {
		t.Fatalf("content the flag never saw is trusted after a reload: %+v", st)
	}
	if err := GrantReviewed(ws, st.Reviewed()); err != nil {
		t.Fatal(err)
	}
	if st = RefreshAgents(cfg.Workspace); !st.AgentsTrusted || st.AgentsReason != "stored" {
		t.Fatalf("a stored grant for the new content: %+v", st)
	}
}

// A decision about the file alone keeps the stored decision about the
// definitions, as abhed init records one.
func TestConfigDecisionKeepsAgentsDecision(t *testing.T) {
	_, ws := trustHome(t, "", "")
	writeAgent(t, ws, "reviewer.md", reviewerDef)
	st, _ := InspectWorkspace(ws)
	if err := GrantReviewed(ws, st.Reviewed()); err != nil {
		t.Fatal(err)
	}
	if _, err := InitWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	st, _ = InspectWorkspace(ws)
	if !st.Trusted || !st.AgentsTrusted || st.AgentsReason != "stored" {
		t.Fatalf("abhed init forgot the definitions' trust: %+v", st)
	}
	// And a decision declining the file keeps trusted definitions trusted.
	if err := RecordDecision(ws, Reviewed{SHA256: st.SHA256, AgentsSHA256: st.AgentsSHA256}, false, true); err != nil {
		t.Fatal(err)
	}
	if st, _ = InspectWorkspace(ws); st.Trusted || !st.AgentsTrusted {
		t.Fatalf("declining the file: %+v", st)
	}
}

// At most 64 definitions are read, and none larger than 64 KiB.
func TestAgentFileCaps(t *testing.T) {
	_, ws := trustHome(t, "", "")
	for i := range maxAgentFiles + 3 {
		writeAgent(t, ws, fmt.Sprintf("a%03d.md", i), reviewerDef)
	}
	st, _ := InspectWorkspace(ws)
	if len(st.Agents) != maxAgentFiles || !strings.Contains(strings.Join(st.AgentsProblems, "\n"), "more than 64 definitions") {
		t.Fatalf("read %d definitions, problems %v", len(st.Agents), st.AgentsProblems)
	}

	_, ws = trustHome(t, "", "")
	writeAgent(t, ws, "big.md", reviewerDef+strings.Repeat("x", maxAgentFileBytes))
	writeAgent(t, ws, "edge.md", reviewerDef+strings.Repeat("x", maxAgentFileBytes-len(reviewerDef)))
	st, _ = InspectWorkspace(ws)
	if len(st.Agents) != 1 || st.Agents[0] != ".abhed/agents/edge.md" ||
		!strings.Contains(strings.Join(st.AgentsProblems, "\n"), "big.md: larger than 64 KiB") {
		t.Fatalf("size cap: agents %v, problems %v", st.Agents, st.AgentsProblems)
	}
}

// An untrusted workspace may only lower the background limits and tighten
// the wake mode; never raise or loosen them.
func TestWorkspaceCannotRaiseWake(t *testing.T) {
	for _, c := range []struct {
		file string
		want func(Config) bool
	}{
		{`{"subagents":{"wake":"auto"}}`, func(c Config) bool { return c.Subagents.Wake == "notify" }},
		{`{"subagents":{"wake":"off"}}`, func(c Config) bool { return c.Subagents.Wake == "off" }},
		{`{"subagents":{"max_wakes_per_hour":40}}`, func(c Config) bool { return c.Subagents.MaxWakesPerHour == 4 }},
		{`{"subagents":{"max_wakes_per_hour":0}}`, func(c Config) bool { return c.Subagents.MaxWakesPerHour == 0 }},
		{`{"subagents":{"wake_max_turns":50}}`, func(c Config) bool { return c.Subagents.WakeMaxTurns == 8 }},
		{`{"subagents":{"wake_max_turns":2}}`, func(c Config) bool { return c.Subagents.WakeMaxTurns == 2 }},
		{`{"limits":{"max_background_subagents":9}}`, func(c Config) bool { return c.Limits.MaxBackgroundSubagents == 4 }},
		{`{"limits":{"max_background_subagents":0}}`, func(c Config) bool { return c.Limits.MaxBackgroundSubagents == 0 }},
		{`{"limits":{"background_max_minutes":480}}`, func(c Config) bool { return c.Limits.BackgroundMaxMinutes == 60 }},
		{`{"limits":{"background_max_minutes":10}}`, func(c Config) bool { return c.Limits.BackgroundMaxMinutes == 10 }},
	} {
		_, ws := trustHome(t, "", c.file)
		cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
		if err != nil {
			t.Fatal(err)
		}
		if !c.want(cfg) {
			t.Fatalf("%s gave wake %q, wakes %d, turns %d, live %d, minutes %d", c.file, cfg.Subagents.Wake,
				cfg.Subagents.MaxWakesPerHour, cfg.Subagents.WakeMaxTurns, cfg.Limits.MaxBackgroundSubagents, cfg.Limits.BackgroundMaxMinutes)
		}
	}
}

// A file swapped in between the check on the path and the open is refused:
// the open file must be the one that was checked.
func TestAgentFileSwappedAfterCheckRefused(t *testing.T) {
	dir := t.TempDir()
	checked := filepath.Join(dir, "a.md")
	swapped := filepath.Join(dir, "b.md")
	for _, p := range []string{checked, swapped} {
		if err := os.WriteFile(p, []byte(reviewerDef), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := openAgent
	openAgent = func(string) (*os.File, error) { return os.Open(swapped) }
	defer func() { openAgent = old }()
	if _, err := ReadAgentFile(checked); err == nil || !strings.Contains(err.Error(), "changed while it was read") {
		t.Fatalf("a swapped file was read: %v", err)
	}
}
