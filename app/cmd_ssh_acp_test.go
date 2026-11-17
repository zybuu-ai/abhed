package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ssh-acp refuses before it connects: a malformed account, an unpinned or
// relative engine, a relative folder, a local build without the pinned hash,
// and any run inside an agent's command.
func TestSSHACPRefusals(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	build := filepath.Join(t.TempDir(), "abhed-linux")
	if err := os.WriteFile(build, []byte("not this hash"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		args []string
		want string
	}{
		"no folder":       {[]string{"-host", "h", "-user", "u", "-sha256", sha, "-engine", "/opt/abhed"}, "usage"},
		"bad user":        {[]string{"-host", "h", "-user", "-oProxyCommand=x", "-sha256", sha, "-engine", "/opt/abhed", "--", "/w"}, "SSH account"},
		"bad host":        {[]string{"-host", "-oProxyCommand=x", "-user", "u", "-sha256", sha, "-engine", "/opt/abhed", "--", "/w"}, "SSH account"},
		"no pin":          {[]string{"-host", "h", "-user", "u", "-engine", "/opt/abhed", "--", "/w"}, "SHA-256"},
		"relative folder": {[]string{"-host", "h", "-user", "u", "-sha256", sha, "-engine", "/opt/abhed", "--", "w"}, "absolute path on the host"},
		"relative engine": {[]string{"-host", "h", "-user", "u", "-sha256", sha, "-engine", "abhed", "--", "/w"}, "-engine is an absolute"},
		"no engine":       {[]string{"-host", "h", "-user", "u", "-sha256", sha, "--", "/w"}, "give -engine"},
		"upload mismatch": {[]string{"-host", "h", "-user", "u", "-sha256", sha, "-upload", build, "--", "/w"}, "does not have the pinned SHA-256"},
	}
	for name, c := range cases {
		var out, errw bytes.Buffer
		if code := sshACPCmd(c.args, strings.NewReader(""), &out, &errw); code == 0 || !strings.Contains(errw.String(), c.want) {
			t.Errorf("%s: %d %q", name, code, errw.String())
		}
	}
	old := inAgentCommand
	t.Cleanup(func() { inAgentCommand = old })
	inAgentCommand = func() string { return "ABHED_SANDBOX is set" }
	var errw bytes.Buffer
	if code := sshACPCmd([]string{"-host", "h", "-user", "u", "-sha256", sha, "-engine", "/opt/abhed", "--", "/w"}, strings.NewReader(""), &bytes.Buffer{}, &errw); code == 0 || !strings.Contains(errw.String(), "inside an agent") {
		t.Fatalf("inside an agent: %q", errw.String())
	}
}
