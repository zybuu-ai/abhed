package config

import (
	"slices"
	"strings"
	"testing"
)

// stricterListen lets a workspace file only tighten sandbox.listen, for
// every pair of the user's and the workspace's values.
func TestWorkspaceListenOnlyTightens(t *testing.T) {
	rank := map[string]int{"allow": 0, "": 1, "ask": 1, "off": 2}
	for _, user := range []string{"", "ask", "allow", "off"} {
		for _, ws := range []string{"", "ask", "allow", "off"} {
			dst := Config{}
			dst.Sandbox.Listen = user
			w := Config{}
			w.Sandbox.Listen = ws
			applied := stricterListen(&dst, &w)
			want := rank[ws] > rank[user]
			if applied != want || applied && dst.Sandbox.Listen != ws || !applied && dst.Sandbox.Listen != user {
				t.Fatalf("user %q, workspace %q: applied %v, now %q", user, ws, applied, dst.Sandbox.Listen)
			}
			if dst.Sandbox.Listen == "allow" && user != "allow" {
				t.Fatalf("a workspace file turned listening to allow from %q", user)
			}
		}
	}
}

func TestValidateListen(t *testing.T) {
	for _, c := range []struct {
		listen string
		ports  []string
		bad    string
	}{
		{"", nil, ""},
		{"ask", []string{"3000-3009", "5173"}, ""},
		{"allow", nil, ""},
		{"off", nil, ""},
		{"loopback", nil, "sandbox.listen"},
		{"", []string{"80"}, "sandbox.listen_ports"},
		{"", []string{"3000-2999"}, "sandbox.listen_ports"},
		{"", []string{"x"}, "sandbox.listen_ports"},
		{"", []string{"2000-3000"}, "sandbox.listen_ports"},
	} {
		cfg := Default()
		cfg.Sandbox.Listen, cfg.Sandbox.ListenPorts = c.listen, c.ports
		err := cfg.Validate()
		if c.bad == "" && err != nil || c.bad != "" && (err == nil || !strings.Contains(err.Error(), c.bad)) {
			t.Fatalf("%q %v: %v", c.listen, c.ports, err)
		}
	}
}

// The default dev ports leave out common database, cache and console ports.
func TestDefaultListenPorts(t *testing.T) {
	ports, err := ParseListenPorts(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []int{3000, 3009, 4200, 4321, 5173, 5179, 8000, 8009} {
		if !slices.Contains(ports, p) {
			t.Fatalf("%d is not a default listen port", p)
		}
	}
	for _, p := range []int{5432, 3306, 6379, 27017, 5000, 7000, 8080, 8090, 8888, 9000, 11434} {
		if slices.Contains(ports, p) {
			t.Fatalf("%d is a default listen port", p)
		}
	}
	if got, _ := ParseListenPorts([]string{"3001", "3000-3002"}); !slices.Equal(got, []int{3000, 3001, 3002}) {
		t.Fatalf("%v", got)
	}
}
