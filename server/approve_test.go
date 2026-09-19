package server

import (
	"context"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// TestApproveRemembersAlwaysAllow verifies that a reply carrying a scope is
// remembered for the session, so a later call matching the same scope is
// auto-approved without raising another prompt — the fix for default mode
// re-prompting on every mutating call.
func TestApproveRemembersAlwaysAllow(t *testing.T) {
	live := &liveSession{
		approvals: make(chan approvalReply, 1),
		allowed:   map[string]bool{},
	}
	res := policy.Result{Scope: "bash(npm install *)"}

	// First call: reviewer chooses "always allow", which sends the scope back.
	live.approvals <- approvalReply{Approved: true, Scope: res.Scope}
	ok, err := live.Approve(context.Background(), "bash", nil, res)
	if err != nil || !ok {
		t.Fatalf("first approval should accept: ok=%v err=%v", ok, err)
	}
	if !live.allowed[res.Scope] {
		t.Fatalf("scope was not remembered")
	}

	// Second call with the same scope must auto-allow. The channel is empty, so
	// if Approve tried to read it the test would block — proving it did not ask.
	ok, err = live.Approve(context.Background(), "bash", nil, res)
	if err != nil || !ok {
		t.Fatalf("remembered scope should auto-approve: ok=%v err=%v", ok, err)
	}
	if live.State == "waiting_approval" {
		t.Fatalf("auto-approved call must not enter waiting_approval")
	}
}

// TestApproveWithoutScopeIsNotRemembered ensures a plain one-off approval does
// not widen into a standing allowance.
func TestApproveWithoutScopeIsNotRemembered(t *testing.T) {
	live := &liveSession{
		approvals: make(chan approvalReply, 1),
		allowed:   map[string]bool{},
	}
	res := policy.Result{Scope: "bash(npm install *)"}
	live.approvals <- approvalReply{Approved: true} // no scope: approve once only
	if ok, err := live.Approve(context.Background(), "bash", nil, res); err != nil || !ok {
		t.Fatalf("approval should accept: ok=%v err=%v", ok, err)
	}
	if live.allowed[res.Scope] {
		t.Fatalf("a scopeless approval must not be remembered as always-allow")
	}
}
