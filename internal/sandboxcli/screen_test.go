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
