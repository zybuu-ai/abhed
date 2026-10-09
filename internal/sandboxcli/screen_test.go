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

// exec -i with a shell has each statement it is fed judged by the server's
// deny rules: a script whose statements pass reaches the shell whole, and
// one with a refused statement stops there, with nothing after it sent.
func TestExecStdinShellScreened(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	script := "mkdir -p d\nif [ -d d ]; then\n  echo ok\nfi\n"
	res := r.runWith(t, runOpts{in: strings.NewReader(script)}, "exec", "-i", sb.ID, "--", "sh")
	if res.code != 0 || !strings.Contains(res.out, script) {
		t.Fatalf("exec -i sh: %+v", res)
	}
	res = r.runWith(t, runOpts{in: strings.NewReader("echo before\nrm -rf d\necho after\n")}, "exec", "-i", sb.ID, "--", "sh")
	// It ran, and did not end with a status of its own.
	if res.code != ExitRemote || !strings.Contains(res.err, "refused") || !strings.Contains(res.out, "before") ||
		strings.Contains(res.out, "rm -rf") || strings.Contains(res.out, "after") {
		t.Fatalf("exec -i sh with a refused statement: %+v", res)
	}
	// An interpreter that is not a shell is still refused fed stdin.
	res = r.runWith(t, runOpts{in: strings.NewReader("print(1)\n")}, "exec", "-i", sb.ID, "--", "python3")
	if res.code != ExitAuth || !strings.Contains(res.err, "stdin") || !strings.Contains(res.err, ":attach") {
		t.Fatalf("exec -i python3: %+v", res)
	}
}
