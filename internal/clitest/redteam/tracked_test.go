package redteam

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// invariant is one rule the CLI's governance rests on, the pseudo-terminal
// test that proves it end to end, and the checks that hold it today. A check
// is "pkg.TestName", with pkg the directory from the module root.
type invariant struct {
	name   string
	e2e    string
	checks []string
	// pending names what the checks cannot cover yet, and what owns it.
	pending string
}

// harness is what every end-to-end test waits for.
const harness = "the pty harness, owned by internal/clitest"

var invariants = []invariant{
	{"deny wins in every mode", "TestInvariantDenyWinsInEveryMode", []string{
		"app.TestRedTeamDenyWinsInEveryMode", "internal/policy.TestSessionAllowCannotLiftDenyDestructiveAskOrPlan",
		"internal/extension.TestHookAskNeverLiftsADeny"}, harness},
	{"approvals are never auto-granted", "TestInvariantNoAutoApprove", []string{
		"app.TestRedTeamInputEndingRefuses", "app.TestRedTeamHookAllowApprovesNothing",
		"internal/policy.TestHookAllowIsNoOpinionAndAskWaitsForDeny", "internal/extension.TestPromptAndPermissionHooksOnlyVeto"},
		harness + "; the arrow-key and number guard, owned by the terminal UI's dialog (internal/ui)"},
	{"destructive actions always confirm", "TestInvariantDestructiveAlwaysConfirms", []string{
		"app.TestRedTeamDestructiveAlwaysConfirms"}, harness},
	{"managed policy wins", "TestInvariantManagedPolicyWins", []string{
		"app.TestRedTeamManagedPolicyWins", "app.TestModeAutoNeedsAYes", "app.TestAddDirIsRefusedUnderAManagedList",
		"app.TestTurnLimitFollowsTheManagedConfiguration", "app.TestPlanDecisionHonoursAManagedMode"}, harness},
	{"@, ! and custom commands go through policy", "TestInvariantMentionsBangAndCommandsGoThroughPolicy", []string{
		"app.TestRedTeamTypedInputRunsAndAttachesNothing"},
		harness + "; @ mentions, ! commands and custom commands, owned by the CLI's input commands"},
	{"/permissions cannot widen past managed policy", "TestInvariantPermissionsCannotWidenPastManaged", []string{
		"app.TestSessionAllowIsRefusedUnderManagedPermissions", "app.TestCLIPermissionsAcrossClear",
		"app.TestClearedRuleIsNotRecordedInTheNextConversation"}, harness},
	{"mode cycling cannot reach auto or bypass", "TestInvariantModeCycleNeverReachesAutoOrBypass", []string{
		"app.TestModeCycleNeverReachesAutoOrBypass", "app.TestModeSetRules", "app.TestPlanDecision"},
		harness + "; the Shift-Tab key, owned by the terminal UI (internal/ui)"},
	{"rewind is a fork", "TestInvariantRewindIsAFork", []string{
		"app.TestCLIForkCarriesIntoNextTask"},
		harness + "; /rewind and a fork at the first message, owned by the CLI's sessions and rewind"},
	{"the record is append-only", "TestInvariantRecordIsAppendOnly", []string{
		"app.TestRedTeamRecordIsAppendOnly"}, harness + "; record verify and prune, owned by the local record (store/local)"},
	{"secrets are redacted", "TestInvariantSecretsAreRedacted", []string{
		"app.TestRedTeamSecretsAreRedacted"}, harness},
	{"workspace trust gates hooks", "TestInvariantUntrustedHooksDoNotRun", []string{
		"config.TestUntrustedWorkspaceIgnoresWhatWidens", "app.TestCLIPromptHookVeto"}, harness},
}

// testNames lists the Test functions declared in the _test.go files of dir.
func testNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				out[fn.Name.Name] = true
			}
		}
	}
	return out
}

// Every invariant has its named end-to-end test here and at least one check
// that runs today, and each one named exists: a renamed or deleted test fails
// this rather than quietly leaving an invariant unguarded.
func TestInvariantsAreTracked(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	here := testNames(t, ".")
	pkgs := map[string]map[string]bool{}
	for _, inv := range invariants {
		if !here[inv.e2e] {
			t.Errorf("%s: no end-to-end test %s", inv.name, inv.e2e)
		}
		if len(inv.checks) == 0 {
			t.Errorf("%s: nothing checks it today", inv.name)
		}
		if !strings.Contains(inv.pending, "owned by ") {
			t.Errorf("%s: pending names no owner", inv.name)
		}
		for _, c := range inv.checks {
			pkg, name, ok := strings.Cut(c, ".")
			if !ok {
				t.Fatalf("%s: check %q is not pkg.TestName", inv.name, c)
			}
			if pkgs[pkg] == nil {
				pkgs[pkg] = testNames(t, filepath.Join(root, filepath.FromSlash(pkg)))
			}
			if !pkgs[pkg][name] {
				t.Errorf("%s: check %s does not exist", inv.name, c)
			}
		}
	}
	for name := range here {
		if strings.HasPrefix(name, "TestInvariant") && name != "TestInvariantsAreTracked" {
			found := false
			for _, inv := range invariants {
				found = found || inv.e2e == name
			}
			if !found {
				t.Errorf("%s is not in the invariant list", name)
			}
		}
	}
}
