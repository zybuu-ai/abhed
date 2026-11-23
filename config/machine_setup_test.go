package config

import (
	"strings"
	"testing"
)

// The person's machine setup is off only where the organisation's
// configuration sets k8s or ssh, or machine_setup false; a managed file that
// sets anything else leaves it on.
func TestMachineSetupOff(t *testing.T) {
	off := false
	on := true
	for _, tc := range []struct {
		name     string
		cfg      Config
		k8s, ssh bool
	}{
		{"no managed file", Config{}, false, false},
		{"managed web search only", Config{Managed: true, ManagedKeys: []string{"web_search.enabled"}}, false, false},
		{"managed k8s.enabled", Config{Managed: true, ManagedKeys: []string{"k8s.enabled"}}, true, false},
		{"managed ssh.hosts", Config{Managed: true, ManagedKeys: []string{"ssh.hosts"}}, false, true},
		{"managed machine_setup false", Config{Managed: true, ManagedKeys: []string{"machine_setup"}, MachineSetup: &off}, true, true},
		{"managed machine_setup true", Config{Managed: true, ManagedKeys: []string{"machine_setup"}, MachineSetup: &on}, false, false},
		{"a user's machine_setup false", Config{MachineSetup: &off, SetKeys: []string{"machine_setup"}}, false, false},
		{"org policy sets k8s", Config{Managed: true, OrgPolicy: OrgPolicyStatus{Keys: []string{"k8s.clusters"}}}, true, false},
	} {
		k8s, ssh := tc.cfg.MachineSetupOff("k8s"), tc.cfg.MachineSetupOff("ssh")
		if (k8s != "") != tc.k8s || (ssh != "") != tc.ssh {
			t.Errorf("%s: k8s %q, ssh %q", tc.name, k8s, ssh)
		}
		if k8s != "" && !strings.Contains(k8s, "ask your administrator") {
			t.Errorf("%s: the reason does not say what to do: %q", tc.name, k8s)
		}
	}
}
