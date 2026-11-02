// Package managed names where the organisation's configuration lives.
//
// It is internal so a test in this module can point it at a temporary file,
// and nothing outside the module can move it.
package managed

import (
	"os"
	"path/filepath"
)

// Every path the organisation's files are read from is here, so a test
// build moves them all together. Each is a constant-initialised variable,
// which -ldflags -X can set.

// ConfigFile is the managed configuration. Every layer below it yields to it.
var ConfigFile = "/etc/abhed/config.json"

// AgentsDir holds the organisation's subagent definitions. They load first
// and no workspace or operator definition can take their names.
var AgentsDir = "/etc/abhed/agents"

// MemoryDir holds the organisation's ABHED.md, read into every system prompt.
var MemoryDir = "/etc/abhed"

// CommandsDir holds the organisation's custom slash commands.
var CommandsDir = "/etc/abhed/commands"

// StylesDir holds the organisation's output styles.
var StylesDir = "/etc/abhed/styles"

// OrgStateFile, when set, replaces ~/.abhed/org-policy.json; tests set it.
var OrgStateFile string

// testDirEnv is set only by the end-to-end test build (-ldflags -X): the
// variable naming an absolute directory for one run's managed files. A
// release build leaves it empty and never reads the environment for this.
var testDirEnv string

func init() { applyTestDir() }

// applyTestDir moves the managed paths to the test build's directory, and
// does nothing in any other build.
func applyTestDir() {
	if testDirEnv == "" {
		return
	}
	if d := os.Getenv(testDirEnv); filepath.IsAbs(d) {
		MoveTo(d)
	}
}

// MoveTo points every managed path under dir: the test build's hook, and
// testenv's isolation.
func MoveTo(dir string) {
	ConfigFile = filepath.Join(dir, "config.json")
	AgentsDir = filepath.Join(dir, "agents")
	MemoryDir = dir
	CommandsDir = filepath.Join(dir, "commands")
	StylesDir = filepath.Join(dir, "styles")
}
