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

// ssh_connect naming a declared host is answered before anyone is asked:
// the host is ready, and the ssh tool is the way to it. A look-alike name
// for another address is still refused as one.
func TestConnectDeclaredHostIsReady(t *testing.T) {
	reg, _ := NewRegistry([]HostConfig{{Name: "app1", Addr: "127.0.0.1:9", User: "root"}})
	tool := ConnectTool{R: reg, Declared: []string{"app1", "db1"}}
	// db1 did not load: it is never called ready.
	if err := tool.Precheck(nil, json.RawMessage(`{"addr":"db1"}`)); err == nil || strings.Contains(err.Error(), "ready") ||
		!strings.Contains(err.Error(), "did not load") {
		t.Errorf("unloaded host: %v", err)
	}
	if d := tool.Description(); !strings.Contains(d, "not loaded") || !strings.Contains(d, "db1") {
		t.Errorf("description does not set db1 apart: %s", d)
	}
	for _, args := range []string{`{"addr":"app1"}`, `{"addr":"app1","name":"app1"}`} {
		err := tool.Precheck(nil, json.RawMessage(args))
		if err == nil || !strings.Contains(err.Error(), "it is ready") || !strings.Contains(err.Error(), "ssh tool") {
			t.Errorf("%s: %v", args, err)
		}
	}
	err := tool.Precheck(nil, json.RawMessage(`{"addr":"10.0.0.5","name":"APP1"}`))
	if err == nil || !strings.Contains(err.Error(), "reads like") {
		t.Errorf("look-alike: %v", err)
	}
	if err := tool.Precheck(nil, json.RawMessage(`{"addr":"10.0.0.5","name":"vm7"}`)); err != nil {
		t.Errorf("an unrelated host is prechecked away: %v", err)
	}
	for _, d := range []string{tool.Description(), Tool{R: reg}.Description()} {
		if !strings.Contains(d, "app1") || !strings.Contains(d, "ready") {
			t.Errorf("description does not list app1 as ready: %s", d)
		}
	}
}
