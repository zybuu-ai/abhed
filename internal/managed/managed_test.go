package managed

import (
	"path/filepath"
	"testing"
)

// A build without the test hook ignores the variable the end-to-end tests
// use: the environment cannot move the managed configuration.
func TestPlainBuildIgnoresTheTestDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ABHED_CLITEST_MANAGED_DIR", dir)
	oldFile, oldDir, oldEnv := ConfigFile, AgentsDir, testDirEnv
	t.Cleanup(func() { ConfigFile, AgentsDir, testDirEnv = oldFile, oldDir, oldEnv })
	if testDirEnv != "" {
		t.Fatalf("this build has the test hook set to %q", testDirEnv)
	}
	applyTestDir()
	if ConfigFile != "/etc/abhed/config.json" || AgentsDir != "/etc/abhed/agents" {
		t.Fatalf("moved to %s, %s", ConfigFile, AgentsDir)
	}
	// With the hook, the same variable moves them: the check above means something.
	testDirEnv = "ABHED_CLITEST_MANAGED_DIR"
	applyTestDir()
	if ConfigFile != filepath.Join(dir, "config.json") {
		t.Fatalf("the hook did not apply: %s", ConfigFile)
	}
	t.Setenv("ABHED_CLITEST_MANAGED_DIR", "relative/dir")
	ConfigFile = oldFile
	applyTestDir()
	if ConfigFile != oldFile {
		t.Fatalf("a relative directory was taken: %s", ConfigFile)
	}
}
