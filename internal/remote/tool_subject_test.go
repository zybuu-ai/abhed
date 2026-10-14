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

// Host names that pass for one another fold to one name; others stay apart.
func TestFoldNameReadsLookAlikesAsOne(t *testing.T) {
	for a, b := range map[string]string{"pr0d": "prod", "buiId1": "build1", "db-1": "db1", "rnail": "mail", "W3B": "web"} {
		if foldName(a) != foldName(b) {
			t.Errorf("%q and %q fold apart: %q, %q", a, b, foldName(a), foldName(b))
		}
	}
	for a, b := range map[string]string{"build1": "build2", "prod": "stage", "db1": "db10"} {
		if foldName(a) == foldName(b) {
			t.Errorf("%q and %q fold together", a, b)
		}
	}
}
