package app

import (
	"context"
	"strings"
	"testing"
)

// §8.1: the doctor's checks come back structured; a model endpoint off this
// machine is not contacted.
func TestStudioDoctor(t *testing.T) {
	r := newStudioRig(t, "")
	var res struct {
		Checks []doctorCheck `json:"checks"`
	}
	r.cl.ok("_abhed/doctor", map[string]any{"cwd": r.ws}, &res)
	ids := map[string]string{}
	for _, c := range res.Checks {
		ids[c.ID] = c.Status
	}
	for _, want := range []string{"config", "trust", "provider", "sandbox", "record", "mcp", "index", "managed", "web_search"} {
		if ids[want] == "" {
			t.Errorf("no %s check: %v", want, res.Checks)
		}
	}
	for _, c := range res.Checks {
		if c.ID == "web_search" && c.Detail != "off; only the managed configuration can enable it" {
			t.Errorf("web search: %q", c.Detail)
		}
	}
	if ids["provider"] != "ok" || ids["record"] != "ok" {
		t.Fatalf("checks: %+v", res.Checks)
	}
	if st, detail := providerReach(context.Background(), "https://api.example.invalid/v1"); st != "ok" || !strings.Contains(detail, "not contacted") {
		t.Fatalf("a remote endpoint: %s %s", st, detail)
	}
}
