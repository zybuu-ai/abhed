package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
)

// A shutdown ends the hijacked streams too, which the HTTP server's own
// Shutdown leaves open, records how each ended, and opens no new one.
func TestSandboxStreamsEndAtShutdown(t *testing.T) {
	r := newWSRig(t, nil, []driver.Capability{driver.CapAttach}, func(o *Options) { o.SandboxSSHKey = testHostKey(t) })
	shellFake(r.cpRig)
	sb := r.ready(t, "alice")
	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":attach", protoAttach)
	if err != nil {
		t.Fatal(err)
	}
	control(t, ws)
	readUntil(t, ws, "$ ")
	sshClient(t, r, sb.ID)

	r.srv.drain()
	c, _ := control(t, ws)
	if c.Type != "error" || c.Error.Code != "unavailable" || !strings.Contains(c.Error.Message, "shutting down") {
		t.Fatalf("attach at shutdown %+v", c)
	}
	if ev := waitAudit(t, r.cpRig, sb.ID, "attach", controlplane.OutcomeFailed); ev.Code != "unavailable" {
		t.Fatalf("attach record %+v", ev)
	}
	if ev := waitAudit(t, r.cpRig, sb.ID, "ssh", controlplane.OutcomeFailed); ev.Code != "unavailable" {
		t.Fatalf("ssh record %+v", ev)
	}
	if _, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":attach", protoAttach); handshakeStatus(t, err) != http.StatusServiceUnavailable {
		t.Fatalf("a stream opened at shutdown: %v", err)
	}
}
