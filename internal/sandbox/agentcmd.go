package sandbox

import (
	"os"
	"path/filepath"
)

// maxAncestors bounds the walk up the process tree.
const maxAncestors = 64

// Replaced in tests.
var (
	lookupEnv      = os.LookupEnv
	ancestorsOf    = ancestorNames
	executableName = func() string {
		exe, err := os.Executable()
		if err != nil {
			return ""
		}
		return filepath.Base(exe)
	}
)

// InAgentCommand says why this process runs inside an Abhed agent's command
// (ABHED_SANDBOX, or an Abhed ancestor), or ""; a refusal, not the boundary.
func InAgentCommand() string {
	if _, ok := lookupEnv("ABHED_SANDBOX"); ok {
		return "ABHED_SANDBOX is set"
	}
	self := executableName()
	for _, name := range ancestorsOf() {
		if name == "abhed" || (self != "" && name == commName(self, len(name))) {
			return "it runs under an Abhed process"
		}
	}
	return ""
}

// commName is name cut to the length the kernel kept of an ancestor's, when
// that length is the kernel's limit (15 bytes on Linux, 16 on macOS).
func commName(name string, kept int) string {
	if kept >= 15 && len(name) > kept {
		return name[:kept]
	}
	return name
}
