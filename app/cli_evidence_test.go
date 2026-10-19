package app

import (
	"context"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// /evidence lists the last answer's claims, opens the record behind one,
// and shows a record entry by seq.
func TestSlashEvidence(t *testing.T) {
	store := agent.NewMemStore()
	rec := agent.NewRecorder(store, "s1", "")
	zero := 0
	rec.Record(agent.EvActionRequested, agent.ActorAgent, agent.Trusted, agent.ActionRequested{CallID: "c1", Tool: "bash", Args: []byte(`{"command":"go test ./..."}`)})
	obs, _ := rec.Record(agent.EvObservation, agent.ActorTool, agent.Untrusted, agent.Observation{CallID: "c1", Tool: "bash", Content: "12 tests passed", ExitCode: &zero})
	rec.Record(agent.EvAnswerVerdict, agent.ActorSystem, agent.Trusted, agent.AnswerVerdict{Mode: agent.VerifyLabel, Source: "text", Contradicted: 1,
		Claims: []agent.ClaimVerdict{{N: 1, Text: "All 15 tests pass.", Verdict: agent.ClaimContradicted, Reason: "it says 15 test",
			Conflict: &agent.Conflict{Seq: obs.Seq, CallID: "c1", Excerpt: "12 tests passed"}, LoadBearing: true}}})
	surf := &scriptSurface{}
	e := &cmdEnv{ui: surf, st: &cliState{store: store, sessionID: "s1"}}

	if _, err := slashEvidence(context.Background(), e, nil); err != nil {
		t.Fatal(err)
	}
	if got := surf.blocks[0].Text; !strings.Contains(got, "0 verified · 0 unverified · 1 contradicted") || !strings.Contains(got, "1 ✗ All 15 tests pass.") {
		t.Fatalf("list: %s", got)
	}
	surf.blocks = nil
	if _, err := slashEvidence(context.Background(), e, []string{"1"}); err != nil {
		t.Fatal(err)
	}
	if len(surf.blocks) != 2 || !strings.Contains(surf.blocks[1].Text, "go test ./...") || !strings.Contains(surf.blocks[1].Text, "12 tests passed") {
		t.Fatalf("claim 1: %+v", surf.blocks)
	}
	if _, err := slashEvidence(context.Background(), e, []string{"7"}); err == nil {
		t.Fatal("a claim that is not there is refused")
	}
	surf.blocks = nil
	slashEvidence(context.Background(), e, []string{"#99"})
	if !strings.Contains(surf.blocks[0].Text, "not in this session's record") {
		t.Fatalf("seq: %+v", surf.blocks)
	}
}

// -verify may only make a managed verify.mode stricter.
func TestVerifyFlagUnderManagedMode(t *testing.T) {
	cfg := config.Default()
	cfg.Verify.Mode = "strict"
	cfg.ManagedKeys = []string{"verify.mode"}
	if _, err := toolset.VerifyFlag(cfg, "label"); err == nil {
		t.Fatal("a flag lowered the managed verify mode")
	}
	if c, err := toolset.VerifyFlag(cfg, "strict"); err != nil || c.Verify.Mode != "strict" {
		t.Fatalf("%v %q", err, c.Verify.Mode)
	}
	if _, err := toolset.VerifyFlag(config.Default(), "maybe"); err == nil {
		t.Fatal("an unknown mode is refused")
	}
	// Unset, the terminal labels and -p does not check.
	if toolset.VerifyMode(config.Default(), true) != agent.VerifyLabel || toolset.VerifyMode(config.Default(), false) != agent.VerifyOff {
		t.Fatal("surface defaults")
	}
	if v, _ := toolset.Verifier(config.Default(), false); v != nil {
		t.Fatal("-p checks nothing unless asked")
	}
	strict := config.Default()
	strict.Verify.Mode = "strict"
	if v, _ := toolset.Verifier(strict, false); v == nil || !v.AnswerTool {
		t.Fatal("strict offers the answer tool")
	}
}
