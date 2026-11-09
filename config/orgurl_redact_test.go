package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/orgpolicy"
)

// An org policy URL holding a password, a token or a signature never reaches
// a denial's reason (which the model, the record and stderr all get), the
// org policy's name in a refusal, or the status line.
func TestOrgPolicyURLSecretsAreRedacted(t *testing.T) {
	const url = "https://u:pw-SECRET1@org.example/p?token=tok-SECRET2&X-Amz-Signature=sig-SECRET3&sig=sig-SECRET4&v=1"
	cfg := Default()
	cfg.ruleLayers = map[string]string{"deny\x00bash(curl*)": LayerManaged}
	cfg.orgRules = map[string]bool{"deny\x00bash(curl*)": true}
	cfg.OrgPolicy = OrgPolicyStatus{Status: orgpolicy.StatusApplied, URL: url, Version: 3, Name: "default"}
	cfg.Permissions.Deny = []string{"bash(curl*)"}

	pol := policy.New(policy.ModeDefault)
	pol.RuleSource = cfg.RuleSource
	if err := pol.AddDeny(cfg.Permissions.Deny...); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"command": "curl x"})
	res := pol.Evaluate("bash", true, args)
	_, where := cfg.RuleSource("deny", "bash(curl*)")
	for name, text := range map[string]string{
		"reason": res.Reason, "where": where, "orgSource": orgSource(url),
		"status": cfg.OrgPolicy.OrgPolicyLine(),
	} {
		for _, secret := range []string{"SECRET1", "SECRET2", "SECRET3", "SECRET4"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s leaks %s: %s", name, secret, text)
			}
		}
		if !strings.Contains(text, "org.example") {
			t.Errorf("%s no longer names the server: %s", name, text)
		}
	}
	if res.Source != LayerOrg {
		t.Fatalf("source %q", res.Source)
	}
}

// A reason shows the org server as scheme, host and path only; the status
// line keeps the URL with every secret-bearing part redacted, a token held
// as the user name and a fragment included.
func TestOrgPolicyURLShapes(t *testing.T) {
	for _, raw := range []string{
		"https://ghp_SECRET1@org.example/p",
		"https://org.example/p?auth=SECRET1&k=SECRET2&code=SECRET3",
		"https://org.example/p#token=SECRET1",
		"https://u:SECRET1@org.example/p?token=SECRET2#SECRET3",
	} {
		cfg := Default()
		cfg.ruleLayers = map[string]string{"deny\x00bash(curl*)": LayerManaged}
		cfg.orgRules = map[string]bool{"deny\x00bash(curl*)": true}
		cfg.OrgPolicy = OrgPolicyStatus{Status: orgpolicy.StatusApplied, URL: raw, Version: 2}
		_, where := cfg.RuleSource("deny", "bash(curl*)")
		if where != "the org policy (https://org.example/p, version 2)" {
			t.Errorf("%s: reason shows %q", raw, where)
		}
		if got := orgSource(raw); got != "the org policy from https://org.example/p" {
			t.Errorf("%s: orgSource %q", raw, got)
		}
		line := cfg.OrgPolicy.OrgPolicyLine()
		for _, secret := range []string{"SECRET1", "SECRET2", "SECRET3"} {
			if strings.Contains(line, secret) {
				t.Errorf("%s: the status line leaks %s: %s", raw, secret, line)
			}
		}
	}
}
