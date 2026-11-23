package config

import (
	"strings"
	"testing"
	"time"
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

// Through Load: a managed file and an org policy document as written.
func TestMachineSetupOffThroughLoad(t *testing.T) {
	for _, tc := range []struct {
		name, managed string
		k8s, ssh      bool
	}{
		{"web search only", `{"web_search":{"enabled":true,"provider":"duckduckgo"}}`, false, false},
		{"k8s enabled", `{"k8s":{"enabled":true,"clusters":[{"name":"orgprod","server":"https://orgprod.example:6443"}]}}`, true, false},
		{"machine_setup false", `{"machine_setup":false}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withManaged(t, tc.managed)
			cfg, err := LoadWith(t.TempDir(), LoadOptions{Warn: func(string) {}})
			if err != nil {
				t.Fatal(err)
			}
			if (cfg.MachineSetupOff("k8s") != "") != tc.k8s || (cfg.MachineSetupOff("ssh") != "") != tc.ssh {
				t.Fatalf("k8s %q ssh %q", cfg.MachineSetupOff("k8s"), cfg.MachineSetupOff("ssh"))
			}
		})
	}
	t.Run("org policy machine_setup false", func(t *testing.T) {
		noManaged(t)
		f := newOrg(t)
		f.write(t, `{"machine_setup":false}`, time.Now())
		cfg, err := LoadWith(t.TempDir(), LoadOptions{Warn: func(string) {}})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MachineSetupOff("k8s") == "" || cfg.MachineSetupOff("ssh") == "" {
			t.Fatalf("the org policy's machine_setup false was not taken: %+v", cfg.OrgPolicy)
		}
	})
	t.Run("org policy k8s", func(t *testing.T) {
		noManaged(t)
		f := newOrg(t)
		f.write(t, `{"k8s":{"enabled":false}}`, time.Now())
		cfg, err := LoadWith(t.TempDir(), LoadOptions{Warn: func(string) {}})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MachineSetupOff("k8s") == "" || cfg.MachineSetupOff("ssh") != "" {
			t.Fatalf("k8s %q ssh %q", cfg.MachineSetupOff("k8s"), cfg.MachineSetupOff("ssh"))
		}
	})
}
