package server

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/wsconn"
)

func TestStdinReader(t *testing.T) {
	for line, want := range map[string]string{
		"cat":                       "",
		"tar -x -C .":               "",
		"wc -l | sort":              "",
		"tr a r":                    "",
		"bash":                      "bash",
		"/bin/sh -s":                "sh",
		"BASH":                      "bash",
		"python3.12 -":              "python",
		"env A=1 nice -n 5 perl":    "perl",
		"cat | sh":                  "sh",
		"echo $(node)":              "node",
		"kubectl exec -i p -- zsh":  "zsh",
		"busybox ash":               "busybox",
		"xargs -0 curl":             "xargs",
		"awk '{system($0)}'":        "awk",
		". /dev/stdin":              ".",
		"f() { ruby; }; f":          "ruby",
		"$SHELL":                    "a program named by an expansion",
		"env $X":                    "a program named by an expansion",
		"/bin/b*sh":                 "a program named by an expansion",
		"$'\\x62ash'":               "a program named by an expansion",
		"'ba''sh'":                  "bash",
		"b\\ash":                    "bash",
		"if then":                   "a line that cannot be read",
		`env "A='" sh -c 'x' "B='"`: "sh",
	} {
		if got := stdinReader(line); got != want {
			t.Errorf("%s: %q, want %q", line, got, want)
		}
	}
}

// An interpreter that is not a shell, or a shell whose input is not the
// stdin given, is refused fed stdin while a rule or hook could refuse some
// commands, on the WebSocket :exec and an SSH exec; other commands, and any
// command where nothing screens, run.
func TestSandboxStdinInterpreterRefused(t *testing.T) {
	rules := map[string]func(*Options){
		"deny": func(o *Options) { o.Config.Permissions.Deny = []string{"bash(curl *)"} },
		"ask":  func(o *Options) { o.Config.Permissions.Ask = []string{"bash(curl *)"} },
	}
	for name, mod := range rules {
		t.Run(name, func(t *testing.T) {
			r := newWSRig(t, nil, []driver.Capability{driver.CapAttach}, mod, func(o *Options) { o.SandboxSSHKey = testHostKey(t) })
			sb := r.ready(t, "alice")
			for _, req := range []string{`{"command":"perl"}`, `{"argv":["/usr/bin/python3","-"]}`, `{"command":"cat | sh -s"}`} {
				ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":exec", protoExec)
				if err != nil {
					t.Fatal(err)
				}
				_ = ws.WriteMessage(wsconn.Text, []byte(req))
				c, _ := control(t, ws)
				// A pipe into a shell the deny rules already refuse; the rest say why.
				if c.Type != "error" || c.Error.Code != "forbidden" || !strings.Contains(c.Error.Message, "stdin") && !strings.Contains(req, "|") {
					t.Fatalf("%s: %+v %+v", req, c, c.Error)
				}
			}
			waitAudit(t, r.cpRig, sb.ID, "exec", controlplane.OutcomeFailed)
			client := sshClient(t, r, sb.ID)
			s, _ := client.NewSession()
			var errOut bytes.Buffer
			s.Stderr = &errOut
			if err := s.Run("python3 -"); err == nil || !strings.Contains(errOut.String(), "stdin") {
				t.Fatalf("ssh exec python3: %v %q", err, errOut.String())
			}
			if r.fake.Calls("exec") != 0 {
				t.Fatal("a refused command ran")
			}
			// Not an interpreter: it runs.
			s, _ = client.NewSession()
			if out, err := s.Output("cat"); err != nil || string(out) != "cat\n" {
				t.Fatalf("ssh exec cat: %q %v", out, err)
			}
		})
	}
	// With nothing that could refuse a command, not even the default rules,
	// a shell may read stdin.
	r := newWSRig(t, nil, nil, func(o *Options) { o.Config.Permissions.Deny, o.Config.Permissions.Ask = nil, nil })
	sb := r.ready(t, "alice")
	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":exec", protoExec)
	if err != nil {
		t.Fatal(err)
	}
	_ = ws.WriteMessage(wsconn.Text, []byte(`{"command":"bash"}`))
	if c, _ := control(t, ws); c.Type != "started" {
		t.Fatalf("no rules: %+v %+v", c, c.Error)
	}
	_ = ws.WriteMessage(wsconn.Text, []byte(`{"type":"eof"}`))
	if c, _ := control(t, ws); c.Type != "exit" {
		t.Fatalf("no rules end: %+v", c)
	}
}
