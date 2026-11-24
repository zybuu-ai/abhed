package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// The command line keeps "always for this repository" for host git's ask
// where Studio keeps it, in the person's state, so a later fetch, here or in
// a new session, is not asked again; a push, a forced push and an ask made
// each time still are, and nothing is kept where the permissions are managed.
func TestCLIKeepsHostGitGrantForTheRepository(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state", "workspace-grants.json")
	old := grantsFile
	grantsFile = func() string { return file }
	defer func() { grantsFile = old }()
	ws := t.TempDir()
	host := policy.Result{Decision: policy.Ask, Step: hostgit.StepHostGit, Reason: "git fetch runs through host git as you", Scope: ws + "\x00aaaa"}
	ask := func(k ui.Keeper, line, input string, res policy.Result) (bool, string) {
		var out bytes.Buffer
		ap := &ui.Approver{In: strings.NewReader(input), Out: &out, Style: ui.NewStyle(&out), Session: ui.NewAllowList(), Keep: k}
		ok, err := ap.Approve(context.Background(), "bash", json.RawMessage(`{"command":"`+line+`"}`), res)
		if err != nil {
			t.Fatal(err)
		}
		return ok, out.String()
	}
	k := cliKeeper{workspace: ws}
	// Yes, keep, No: the second keeps it.
	ok, shown := ask(k, "git fetch", "2\n", host)
	if !ok || !strings.Contains(shown, "Always allow git fetch, pull and ls-remote from this remote of this repository") {
		t.Fatalf("ok %v:\n%s", ok, shown)
	}
	if b, err := os.ReadFile(file); err != nil || !strings.Contains(string(b), `"family": "git fetch/pull"`) || !strings.Contains(string(b), `"remotes": "aaaa"`) {
		t.Fatalf("kept %s %v", b, err)
	}
	// A new approver, as a new session: answered with no input.
	for line, want := range map[string]bool{"git pull --ff-only": true, "git ls-remote origin": true, "git push origin main": false} {
		if ok, _ := ask(cliKeeper{workspace: ws}, line, "", host); ok != want {
			t.Errorf("%s: allowed %v, want %v", line, ok, want)
		}
	}
	if ok, _ := ask(k, "git fetch", "", policy.Result{Decision: policy.Ask, Step: hostgit.StepHostGitEach}); ok {
		t.Error("a kept grant answered an ask made each time")
	}
	if ok, _ := ask(k, "git fetch", "", policy.Result{Decision: policy.Ask, Step: "ask", Rule: "bash(git fetch*)"}); ok {
		t.Error("a kept grant answered an ask rule")
	}
	if ok, _ := ask(cliKeeper{workspace: t.TempDir()}, "git fetch", "", host); ok {
		t.Error("a grant kept for one workspace answered in another")
	}
	// A repository cloned into the workspace, or this one's remote changed.
	for _, bind := range []string{ws + "/clone\x00aaaa", ws + "\x00bbbb"} {
		other := host
		other.Scope = bind
		if ok, _ := ask(k, "git fetch", "", other); ok {
			t.Errorf("the grant answered for %q", bind)
		}
	}
	if ok, _ := ask(cliKeeper{workspace: ws, locked: true}, "git fetch", "", host); ok {
		t.Error("a kept grant answered where the permissions are managed")
	}
	if _, shown := ask(cliKeeper{workspace: ws, locked: true}, "git fetch", "3\n", host); strings.Contains(shown, "Always allow") {
		t.Errorf("offered where the permissions are managed:\n%s", shown)
	}
}

// A guest, who keeps nothing past the run, is offered no "always".
func TestCLIOffersAGuestNoAlways(t *testing.T) {
	principalMu.Lock()
	old := principal
	principal = identity.Principal{Kind: identity.Guest}
	principalMu.Unlock()
	t.Cleanup(func() { principalMu.Lock(); principal = old; principalMu.Unlock() })
	ws := t.TempDir()
	res := policy.Result{Decision: policy.Ask, Step: hostgit.StepHostGit, Scope: ws + "\x00aaaa"}
	if label := (cliKeeper{workspace: ws}).Offer("bash", json.RawMessage(`{"command":"git fetch"}`), res); label != "" {
		t.Fatalf("a guest was offered %q", label)
	}
	if opts := bashGrantOptions("git fetch", ws, ws, "b", res); len(opts) != 0 {
		t.Fatalf("a guest was offered %v in Studio", opts)
	}
}

// A run with nobody to ask (-p, rpc without approvals) still uses a grant
// the person kept for this repository, as guide 04 says, and refuses the
// rest; with approvals the rest goes to the caller.
func TestHeadlessUsesTheKeptHostGitGrant(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state", "workspace-grants.json")
	old := grantsFile
	grantsFile = func() string { return file }
	defer func() { grantsFile = old }()
	ws := t.TempDir()
	host := policy.Result{Decision: policy.Ask, Step: hostgit.StepHostGit, Reason: "git fetch runs through host git as you", Scope: ws + "\x00aaaa"}
	fetch := json.RawMessage(`{"command":"git fetch"}`)
	headless := keptApprover{keep: cliKeeper{workspace: ws}, next: agent.AutoApprove{Yes: false}}
	if ok, _ := headless.Approve(context.Background(), "bash", fetch, host); ok {
		t.Fatal("approved with nothing kept and nobody to ask")
	}
	g, ok := cliKeeper{workspace: ws}.grantOf("bash", fetch, host)
	if !ok {
		t.Fatal("no grant for host git's ask")
	}
	if err := keepGrant(ws, g); err != nil {
		t.Fatal(err)
	}
	if ok, _ := headless.Approve(context.Background(), "bash", json.RawMessage(`{"command":"git pull"}`), host); !ok {
		t.Fatal("a headless run did not use the kept grant")
	}
	if ok, _ := headless.Approve(context.Background(), "bash", json.RawMessage(`{"command":"git push origin main"}`), host); ok {
		t.Fatal("the kept fetch grant answered a push")
	}
	locked := keptApprover{keep: cliKeeper{workspace: ws, locked: true}, next: agent.AutoApprove{Yes: false}}
	if ok, _ := locked.Approve(context.Background(), "bash", fetch, host); ok {
		t.Fatal("a kept grant answered where the permissions are managed")
	}
	asked := 0
	withCaller := keptApprover{keep: cliKeeper{workspace: ws}, next: approverFunc(func(context.Context, string, json.RawMessage, policy.Result) (bool, error) {
		asked++
		return false, nil
	})}
	_, _ = withCaller.Approve(context.Background(), "bash", fetch, host)
	_, _ = withCaller.Approve(context.Background(), "bash", json.RawMessage(`{"command":"git push origin main"}`), host)
	if asked != 1 {
		t.Fatalf("the caller was asked %d times, want only for the push", asked)
	}
}

// The reviewer's case, at the approver: a push grant kept for a remote's
// scope, then remote.origin.mirror set, which binds the push to another scope
// (internal/hostgit TestPushScopeBindsMirrorAndPushRefspecs); a headless
// push asked under that scope is not approved by the kept grant.
func TestKeptPushGrantStopsAtAMirror(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state", "workspace-grants.json")
	old := grantsFile
	grantsFile = func() string { return file }
	defer func() { grantsFile = old }()
	ws := t.TempDir()
	push := json.RawMessage(`{"command":"git push origin main"}`)
	at := func(digest string) policy.Result {
		return policy.Result{Decision: policy.Ask, Step: hostgit.StepHostGit, Reason: "git push runs through host git as you", Scope: ws + "\x00" + digest}
	}
	g, ok := cliKeeper{workspace: ws}.grantOf("bash", push, at("plain0000000000000000000"))
	if !ok {
		t.Fatal("no grant for host git's push ask")
	}
	if err := keepGrant(ws, g); err != nil {
		t.Fatal(err)
	}
	headless := keptApprover{keep: cliKeeper{workspace: ws}, next: agent.AutoApprove{Yes: false}}
	if ok, _ := headless.Approve(context.Background(), "bash", push, at("plain0000000000000000000")); !ok {
		t.Fatal("the kept push grant did not answer the remote it was kept for")
	}
	if ok, _ := headless.Approve(context.Background(), "bash", push, at("mirror000000000000000000")); ok {
		t.Fatal("a kept push grant approved a headless push to a remote made a mirror since")
	}
}
