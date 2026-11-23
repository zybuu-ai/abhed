//go:build unix

package clitest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An org policy URL's secrets (a user part, a query value, a fragment) reach
// no surface: not the fallback warning, the record's session.started, abhed
// doctor, or abhed policy status. The server is still named.
func TestOrgPolicyURLSecretsReachNoSurface(t *testing.T) {
	t.Parallel()
	const url = "https://orguser:SECRETPW@127.0.0.1:9/pol/path?token=SECRETQ&x=1#SECRETFRAG"
	h := piped(t, Opts{Managed: `{"org_policy":{"url":"` + url + `","required":true}}`,
		Args: []string{"-p", "hi"}, Script: `text "done"`})
	leaks := func(where, text string) {
		t.Helper()
		for _, s := range []string{"SECRET", "orguser"} {
			if strings.Contains(text, s) {
				t.Errorf("%s shows %q:\n%s", where, s, text)
			}
		}
	}
	if !strings.Contains(h.Stderr(), "127.0.0.1:9/pol/path") {
		t.Fatalf("the warning does not name the server:\n%s", h.Stderr())
	}
	leaks("the -p warning", h.Stderr())
	var rec strings.Builder
	_ = filepath.WalkDir(filepath.Join(h.Home(), ".abhed"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
			b, _ := os.ReadFile(p)
			rec.Write(b)
		}
		return nil
	})
	if !strings.Contains(rec.String(), `"org_policy"`) {
		t.Fatal("no record carries org_policy")
	}
	leaks("the record", rec.String())
	for _, args := range [][]string{{"doctor"}, {"policy", "status"}} {
		cmd := exec.Command(Binary(t), args...)
		cmd.Dir = h.Workspace()
		cmd.Env = h.env()
		out, _ := cmd.CombinedOutput()
		if !strings.Contains(string(out), "127.0.0.1:9") {
			t.Errorf("abhed %s does not name the server:\n%s", strings.Join(args, " "), out)
		}
		leaks("abhed "+strings.Join(args, " "), string(out))
	}
}
