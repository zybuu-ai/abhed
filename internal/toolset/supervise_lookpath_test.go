package toolset

import (
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
