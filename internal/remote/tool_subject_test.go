package remote

import (
	"encoding/json"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// ssh is judged on its host and command, so an always-allow for one host
// never covers another.
func TestSSHPolicySubjectIsHostFirst(t *testing.T) {
	raw := json.RawMessage(`{"host":"prod","command":"uptime"}`)
	if got := policy.Subject("ssh", raw); got != "prod/uptime" {
		t.Fatalf("subject %q", got)
	}
	e := policy.New(policy.ModeDefault)
	if err := e.AddAllow("ssh(staging/*)"); err != nil {
		t.Fatal(err)
	}
	if res := e.Evaluate("ssh", true, raw); res.Decision == policy.Allow {
		t.Fatal("an allow rule for staging allowed prod")
	}
}
