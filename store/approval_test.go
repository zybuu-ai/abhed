package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func approvalStore(t *testing.T, tenant string) (*Postgres, context.Context) {
	t.Helper()
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run approval tests")
	}
	ctx := context.Background()
	cfg := singleRoleConfig(dsn)
	cfg.Tenant = tenant
	pg, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg, ctx
}

// The point of the table: a decision written by one connection is visible to
// another. Without this an answer arriving at the wrong node is lost.
func TestAnswerIsVisibleToTheWaitingNode(t *testing.T) {
	waiting, ctx := approvalStore(t, "default")
	answering, _ := approvalStore(t, "default")

	id, err := waiting.AskApproval(ctx, Approval{
		SessionID: "s-ap-1", Tool: "bash", Reason: "destructive", Scope: "bash(rm*)",
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}

	if _, answered, _ := waiting.ApprovalResult(ctx, id); answered {
		t.Fatal("reported answered before anyone answered")
	}

	ok, err := answering.AnswerApproval(ctx, id, true, "reviewer")
	if err != nil || !ok {
		t.Fatalf("answer: ok=%v err=%v", ok, err)
	}

	approved, answered, err := waiting.ApprovalResult(ctx, id)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if !answered || !approved {
		t.Fatalf("waiting node saw answered=%v approved=%v, want true/true", answered, approved)
	}
}

// A second click must not overturn the first, and a stale browser must not
// answer a question that has moved on.
func TestAnAnsweredApprovalCannotBeAnsweredAgain(t *testing.T) {
	pg, ctx := approvalStore(t, "default")
	id, err := pg.AskApproval(ctx, Approval{SessionID: "s-ap-2", Tool: "bash"})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if ok, _ := pg.AnswerApproval(ctx, id, false, "first"); !ok {
		t.Fatal("first answer was refused")
	}
	if ok, _ := pg.AnswerApproval(ctx, id, true, "second"); ok {
		t.Fatal("ATTACK SUCCEEDED: a denial was overturned by answering twice")
	}
	approved, answered, _ := pg.ApprovalResult(ctx, id)
	if !answered || approved {
		t.Fatalf("result is answered=%v approved=%v, want the first answer to stand", answered, approved)
	}
}

// A pending approval is findable by session, which is how a node that does
// not hold the session still knows what is being asked.
func TestPendingIsFoundBySession(t *testing.T) {
	pg, ctx := approvalStore(t, "default")
	if _, err := pg.AskApproval(ctx, Approval{
		SessionID: "s-ap-3", Tool: "write", Reason: "mutating",
	}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	got, found, err := pg.PendingApproval(ctx, "s-ap-3")
	if err != nil || !found {
		t.Fatalf("pending: found=%v err=%v", found, err)
	}
	if got.Tool != "write" {
		t.Fatalf("tool = %q, want write", got.Tool)
	}
	// Once answered it is no longer pending.
	if _, err := pg.AnswerApproval(ctx, got.ID, true, "r"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if _, found, _ := pg.PendingApproval(ctx, "s-ap-3"); found {
		t.Fatal("an answered approval is still reported as pending")
	}
}

// An approval belongs to a tenant. Another tenant must not see it, let alone
// answer it.
func TestApprovalsAreTenantIsolated(t *testing.T) {
	victim, ctx := approvalStore(t, "t-victim")
	attacker, _ := approvalStore(t, "t-attacker")

	id, err := victim.AskApproval(ctx, Approval{SessionID: "s-ap-4", Tool: "bash"})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if _, found, _ := attacker.PendingApproval(ctx, "s-ap-4"); found {
		t.Fatal("ATTACK SUCCEEDED: another tenant read a pending approval")
	}
	if ok, _ := attacker.AnswerApproval(ctx, id, true, "attacker"); ok {
		t.Fatal("ATTACK SUCCEEDED: another tenant answered an approval")
	}
	if _, answered, _ := victim.ApprovalResult(ctx, id); answered {
		t.Fatal("ATTACK SUCCEEDED: the approval was answered across the tenant boundary")
	}
	_ = time.Now
}
