package toolset

import (
	"context"
	"os/exec"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// The supervisor finds a program where a command does: on the host's PATH,
// or first on PATH from the toolchains Abhed installed for the sandbox.
func TestSupervisorFindsToolchainPrograms(t *testing.T) {
	b := tools.Bash{Isolation: tools.Isolation{Tier: "process"},
		Provides: func(_, p string) bool { return p == "gopls" }}
	find := ProgramFinder(tools.NewRegistry(b))
	if find == nil {
		t.Fatal("no finder on the process tier")
	}
	if !find("gopls") || !find("sh") || find("abhed-no-such-program-x1") {
		t.Fatal("the finder does not match the PATH commands get")
	}
	b.Isolation.Tier = "container"
	if ProgramFinder(tools.NewRegistry(b)) != nil {
		t.Fatal("a container tier's programs were judged from the host")
	}
}

// The CLI's tier is known only when its first command runs (RanUnder): a
// language server that is not installed is still found missing then on the
// tiers that run commands on the host's files, and left to the start in a
// container.
func TestProgramFinderAtRunTime(t *testing.T) {
	tier := "fence"
	b := tools.Bash{Isolation: tools.Isolation{Tier: "process"}, RanUnder: func() string { return tier },
		Sandbox: func(context.Context, string, string) *exec.Cmd { return nil }}
	find := ProgramFinder(tools.NewRegistry(b))
	if find == nil {
		t.Fatal("no finder where the tier is chosen at run time")
	}
	if !find("sh") || find("basedpyright-langserver-abhed-no-such") {
		t.Fatal("the finder does not match the host's PATH")
	}
	tier = "container"
	if !find("basedpyright-langserver-abhed-no-such") {
		t.Fatal("a container's programs were judged from the host")
	}
}
