package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A token leaked through the workspace cannot carry another call's traffic:
// each call has its own way in; a credential-less client is its own call.
func TestEgressCallsCannotBorrowAnotherCallsToken(t *testing.T) {
	s, ws := egressProcess(t)
	var rec eventLog
	x := Launch{CallID: "call-x", Session: "sess-T", Record: rec.record}
	y := Launch{CallID: "call-y", Session: "sess-T", Record: rec.record}
	// x stays live while y runs, so its token is valid throughout.
	live, keep := context.WithCancel(context.Background())
	defer keep()
	if out, err := s.Command(WithLaunch(live, x), ws, `printf %s "$HTTP_PROXY" > tok`).CombinedOutput(); err != nil {
		t.Fatalf("x: %v %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "tok")); !strings.Contains(string(b), "call-x") {
		t.Fatalf("x's proxy URL: %q", b)
	}
	_ = runLaunched(t, s, ws, y, `curl -sS -m 20 -o /dev/null -x "$(cat tok)" http://denied.test/borrowed`)
	_ = runLaunched(t, s, ws, y, `curl -sS -m 20 -o /dev/null -x "http://127.0.0.1:${HTTP_PROXY##*:}" http://denied.test/nocred`)
	nocred := rec.waitFor(t, "the uncredentialed request", func(e map[string]any) bool { return e["path"] == "/nocred" })
	if nocred["call_id"] != "call-y" {
		t.Fatalf("a request without credentials was recorded as %v", nocred)
	}
	time.Sleep(300 * time.Millisecond)
	for _, e := range rec.find(func(e map[string]any) bool { return e["path"] == "/borrowed" }) {
		if e["call_id"] == "call-x" {
			t.Fatalf("a borrowed token moved y's traffic to x: %v", e)
		}
	}
}
