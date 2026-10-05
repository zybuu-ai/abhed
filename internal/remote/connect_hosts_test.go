package remote

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Under ssh.connect_hosts, ssh_connect reaches only a listed address; one
// elsewhere is refused before any connection is tried.
func TestConnectHostsBoundSSHConnect(t *testing.T) {
	for addr, want := range map[string]bool{
		"10.0.0.5": true, "10.0.0.5:2222": true, "VM.Lab.Example": true, "vm.lab.example:22": true,
		"evil.example": false, "10.0.0.50": false, "lab.example.evil.net": false,
	} {
		if got := addrAllowed([]string{"10.0.0.5", "*.lab.example"}, addr); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
	if !addrAllowed(nil, "anything") {
		t.Fatal("no list allows any address")
	}
	reg, _ := NewRegistry(nil)
	sess := newSession(t)
	args, _ := json.Marshal(map[string]any{"addr": "203.0.113.9", "user": "u", "identity_file": "/nonexistent"})
	res := ConnectTool{R: reg, Allowed: []string{"10.0.0.*"}}.Run(context.Background(), sess, args)
	if !res.IsError || !strings.Contains(res.Content, "ssh.connect_hosts") {
		t.Fatalf("an unlisted address was tried: %+v", res)
	}
}
