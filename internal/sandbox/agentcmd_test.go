package sandbox

import "testing"

func TestInAgentCommand(t *testing.T) {
	oldEnv, oldAnc, oldExe := lookupEnv, ancestorsOf, executableName
	t.Cleanup(func() { lookupEnv, ancestorsOf, executableName = oldEnv, oldAnc, oldExe })
	env := map[string]string{}
	lookupEnv = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	var anc []string
	ancestorsOf = func() []string { return anc }
	executableName = func() string { return "abhed-nightly-build-x" }

	for _, c := range []struct {
		env  bool
		anc  []string
		want bool
	}{
		{false, []string{"zsh", "Terminal", "launchd"}, false},
		{true, nil, true},
		// ABHED_SANDBOX stripped with env -u: an Abhed process is still above.
		{false, []string{"bash", "abhed"}, true},
		// A renamed build, as the kernel cuts its name.
		{false, []string{"sh", "abhed-nightly-bu"}, true},
		{false, []string{"sh", "abhed-nightly-b"}, true},
		{false, []string{"abhed-studio", "Abhed Studio"}, false},
	} {
		delete(env, "ABHED_SANDBOX")
		if c.env {
			env["ABHED_SANDBOX"] = "none"
		}
		anc = c.anc
		if got := InAgentCommand() != ""; got != c.want {
			t.Errorf("env %v ancestors %v: %v, want %v", c.env, c.anc, got, c.want)
		}
	}
}

// The real walk finds this test's own parent chain without error.
func TestAncestorNamesReadsTheTree(t *testing.T) {
	names := ancestorNames()
	if processParents() != nil && len(names) == 0 {
		t.Fatal("no ancestors read")
	}
}
