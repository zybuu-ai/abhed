package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/wsconn"
)

// wsRig is a control-plane rig served over a real connection, which a
// WebSocket needs.
type wsRig struct {
	*cpRig
	url string
}

func newWSRig(t *testing.T, values map[string]string, caps []driver.Capability, mods ...func(*Options)) *wsRig {
	t.Helper()
	mods = append([]func(*Options){func(o *Options) { o.StreamRecheck = 50 * time.Millisecond }}, mods...)
	r := buildCPRig(t, values, nil, caps, mods...)
	srv := httptest.NewServer(r.h)
	t.Cleanup(srv.Close)
	return &wsRig{cpRig: r, url: srv.URL}
}

func (r *wsRig) dial(t *testing.T, token, path, proto string, header ...string) (*wsconn.Conn, error) {
	t.Helper()
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return wsconn.Dial(ctx, r.url+path, wsconn.DialOptions{Header: h, Protocols: []string{proto}})
}

// control reads messages until a text one, returning it and the bytes before it.
func control(t *testing.T, ws *wsconn.Conn) (streamControl, []byte) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = ws.SetReadDeadline(time.Time{}) }()
	var data []byte
	for {
		typ, msg, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("reading for a control message: %v (after %q)", err, data)
		}
		if typ == wsconn.Binary {
			data = append(data, msg...)
			continue
		}
		var c streamControl
		if err := json.Unmarshal(msg, &c); err != nil {
			t.Fatalf("control %q: %v", msg, err)
		}
		return c, data
	}
}

// readUntil reads binary messages until their bytes hold want.
func readUntil(t *testing.T, ws *wsconn.Conn, want string) string {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = ws.SetReadDeadline(time.Time{}) }()
	var got []byte
	for !bytes.Contains(got, []byte(want)) {
		typ, msg, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %q: %v (got %q)", want, err, got)
		}
		if typ == wsconn.Binary {
			got = append(got, msg...)
		}
	}
	return string(got)
}

func auditDetail(t *testing.T, r *cpRig, id, action, outcome string) (controlplane.AuditEvent, bool) {
	t.Helper()
	evs, _ := r.store.AuditFor(context.Background(), id)
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Action == action && (outcome == "" || evs[i].Outcome == outcome) {
			return evs[i], true
		}
	}
	return controlplane.AuditEvent{}, false
}

// waitAudit waits for the record to hold action with outcome.
func waitAudit(t *testing.T, r *cpRig, id, action, outcome string) controlplane.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ev, ok := auditDetail(t, r, id, action, outcome); ok {
			return ev
		}
		time.Sleep(10 * time.Millisecond)
	}
	evs, _ := r.store.AuditFor(context.Background(), id)
	t.Fatalf("no %s:%s in %+v", action, outcome, evs)
	return controlplane.AuditEvent{}
}

func handshakeStatus(t *testing.T, err error) int {
	t.Helper()
	var he *wsconn.HandshakeError
	if !errors.As(err, &he) {
		t.Fatalf("not a refused upgrade: %v", err)
	}
	return he.Status
}

// Every streaming verb authenticates, checks the origin and the caller's
// sight of the sandbox before the connection is taken over, and records
// nothing for a refused caller.
func TestSandboxStreamsRefuseBeforeUpgrade(t *testing.T) {
	r := newWSRig(t, nil, []driver.Capability{driver.CapAttach, driver.CapForward},
		func(o *Options) { o.SandboxSSHKey = testHostKey(t) })
	sb := r.ready(t, "alice")
	before := len(actionsOf(t, r.cpRig, sb.ID))
	verbs := []struct{ path, proto string }{
		{":attach", protoAttach}, {":exec", protoExec}, {":forward?port=80", protoForward}, {":ssh", protoSSH},
	}
	for _, v := range verbs {
		path := "/v1/sandboxes/" + sb.ID + v.path
		if _, err := r.dial(t, "", path, v.proto); handshakeStatus(t, err) != http.StatusUnauthorized {
			t.Errorf("%s with no token: %v", v.path, err)
		}
		if _, err := r.dial(t, "eve", path, v.proto); handshakeStatus(t, err) != http.StatusForbidden {
			t.Errorf("%s from another tenant: %v", v.path, err)
		}
		if _, err := r.dial(t, "bob", path, v.proto); handshakeStatus(t, err) != http.StatusNotFound {
			t.Errorf("%s by another owner: %v", v.path, err)
		}
		// Another site's page, with alice's credentials, is refused.
		if _, err := r.dial(t, "alice", path, v.proto, "Origin", "https://evil.example"); handshakeStatus(t, err) != http.StatusForbidden {
			t.Errorf("%s cross-origin: %v", v.path, err)
		}
		// A subprotocol the verb does not speak.
		if _, err := r.dial(t, "alice", path, "abhed.other.v1"); handshakeStatus(t, err) != http.StatusBadRequest {
			t.Errorf("%s wrong subprotocol: %v", v.path, err)
		}
	}
	if got := actionsOf(t, r.cpRig, sb.ID); len(got) != before {
		t.Fatalf("refused streams were recorded: %v", got[before:])
	}
	// The server's own origin is accepted.
	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":attach", protoAttach, "Origin", r.url)
	if err != nil {
		t.Fatalf("same-origin attach: %v", err)
	}
	_ = ws.Close()
	// A driver without forward refuses before the upgrade.
	r2 := newWSRig(t, nil, []driver.Capability{driver.CapAttach})
	sb2 := r2.ready(t, "alice")
	if _, err := r2.dial(t, "alice", "/v1/sandboxes/"+sb2.ID+":forward?port=80", protoForward); handshakeStatus(t, err) != http.StatusNotImplemented {
		t.Fatalf("forward without the capability: %v", err)
	}
}

func actionsOf(t *testing.T, r *cpRig, id string) []string {
	t.Helper()
	evs, _ := r.store.AuditFor(context.Background(), id)
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Action+":"+ev.Outcome)
	}
	return out
}

func testHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// shellFake makes the fake's shell echo its keys, exit with 7 on "exit",
// and note its size.
func shellFake(r *cpRig) {
	r.fake.AttachFunc = func(ctx context.Context, _ string, req driver.AttachRequest) (driver.ExecResult, error) {
		lines := make(chan string)
		go func() {
			defer close(lines)
			buf := make([]byte, 4096)
			for {
				n, err := req.Stdin.Read(buf)
				if n > 0 {
					_, _ = req.Stdout.Write(buf[:n])
					lines <- string(buf[:n])
				}
				if err != nil {
					return
				}
			}
		}()
		_, _ = req.Stdout.Write([]byte("$ "))
		for {
			select {
			case <-ctx.Done():
				return driver.ExecResult{}, ctx.Err()
			case ws := <-req.Resize:
				_, _ = req.Stdout.Write([]byte("size " + strconv.Itoa(int(ws.Rows)) + "x" + strconv.Itoa(int(ws.Cols)) + "\r\n$ "))
			case l, ok := <-lines:
				if !ok {
					return driver.ExecResult{ExitCode: 0}, nil
				}
				if strings.Contains(l, "exit") {
					return driver.ExecResult{ExitCode: 7}, nil
				}
				if strings.ContainsAny(l, "\r\n") {
					_, _ = req.Stdout.Write([]byte("\r\n$ "))
				}
			}
		}
	}
}

func TestSandboxAttachRoundTrip(t *testing.T) {
	r := newWSRig(t, nil, []driver.Capability{driver.CapAttach}, func(o *Options) {
		o.Config.Permissions.Deny = []string{"bash(rm *)"}
	})
	shellFake(r.cpRig)
	sb := r.ready(t, "alice")
	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":attach?rows=24&cols=80", protoAttach)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := control(t, ws); c.Type != "started" || c.Generation != sb.Generation {
		t.Fatalf("first message %+v", c)
	}
	readUntil(t, ws, "$ ")
	// A line the deny rules allow reaches the shell and is recorded.
	_ = ws.WriteMessage(wsconn.Binary, []byte("echo hi\r"))
	readUntil(t, ws, "echo hi")
	in := waitAudit(t, r.cpRig, sb.ID, "attach.input", controlplane.OutcomeOK)
	if in.Principal != "alice" || in.Detail["terminal"] == "" {
		t.Fatalf("input record %+v", in)
	}
	// A refused line never reaches the shell: the terminal says why.
	_ = ws.WriteMessage(wsconn.Binary, []byte("rm -rf /\r"))
	got := readUntil(t, ws, "Denied")
	if strings.Contains(got, "rm -rf") {
		t.Fatalf("the refused line reached the shell: %q", got)
	}
	ref := waitAudit(t, r.cpRig, sb.ID, "attach.refused", controlplane.OutcomeFailed)
	if ref.Principal != "alice" || !strings.Contains(ref.Detail["reason"], "denied") {
		t.Fatalf("refusal record %+v", ref)
	}
	// Resizes reach the driver.
	b, _ := json.Marshal(streamControl{Type: "resize", Rows: 40, Cols: 9})
	_ = ws.WriteMessage(wsconn.Text, b)
	readUntil(t, ws, "size 40x9")
	// The shell's exit status ends the stream.
	_ = ws.WriteMessage(wsconn.Binary, []byte("exit\r"))
	c, _ := control(t, ws)
	if c.Type != "exit" || c.Code == nil || *c.Code != 7 {
		t.Fatalf("end %+v", c)
	}
	ev := waitAudit(t, r.cpRig, sb.ID, "attach", controlplane.OutcomeOK)
	if ev.Detail["exit_code"] != "7" || ev.Detail["rows"] != "24" || ev.Principal != "alice" {
		t.Fatalf("attach record %+v", ev)
	}
}

// Where only a line-by-line terminal holds every rule, no shell is opened.
func TestSandboxAttachRefusedWhereOnlyLinesHold(t *testing.T) {
	for name, mod := range map[string]func(*Options){
		"lines": func(o *Options) { o.Config.Sandbox.Terminal = "lines" },
		"managed deny": func(o *Options) {
			o.Config.Managed = true
			o.Config.ManagedKeys = []string{"permissions.deny"}
			o.Config.Permissions.Deny = []string{"bash(curl *)"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newWSRig(t, nil, []driver.Capability{driver.CapAttach}, mod, func(o *Options) { o.SandboxSSHKey = testHostKey(t) })
			sb := r.ready(t, "alice")
			_, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":attach", protoAttach)
			if handshakeStatus(t, err) != http.StatusForbidden {
				t.Fatalf("attach: %v", err)
			}
			if r.fake.Calls("attach") != 0 {
				t.Fatal("the driver was asked to attach")
			}
			// Over SSH a shell is refused too, while a command still runs, judged.
			client := sshClient(t, r, sb.ID)
			s, _ := client.NewSession()
			var errOut bytes.Buffer
			s.Stderr = &errOut
			_ = s.Shell()
			if err := s.Wait(); err == nil || !strings.Contains(errOut.String(), ":exec") {
				t.Fatalf("ssh shell: %v %q", err, errOut.String())
			}
			s, _ = client.NewSession()
			if out, err := s.Output("echo ok"); err != nil || string(out) != "echo ok\n" {
				t.Fatalf("ssh exec: %q %v", out, err)
			}
		})
	}
}

func TestSandboxExecWithStdin(t *testing.T) {
	r := newWSRig(t, nil, nil, func(o *Options) { o.Config.Permissions.Deny = []string{"bash(rm *)"} })
	sb := r.ready(t, "alice")
	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":exec", protoExec)
	if err != nil {
		t.Fatal(err)
	}
	_ = ws.WriteMessage(wsconn.Text, []byte(`{"command":"cat"}`))
	if c, _ := control(t, ws); c.Type != "started" || c.Generation != sb.Generation {
		t.Fatalf("first message %+v", c)
	}
	_ = ws.WriteMessage(wsconn.Binary, []byte("abc"))
	_ = ws.WriteMessage(wsconn.Text, []byte(`{"type":"eof"}`))
	c, data := control(t, ws)
	// Each output message is led by its stream: 1 for stdout.
	if c.Type != "exit" || *c.Code != 0 || string(data) != "\x01cat\n\x01abc" {
		t.Fatalf("exec end %+v %q", c, data)
	}
	if ev := waitAudit(t, r.cpRig, sb.ID, "exec", controlplane.OutcomeOK); ev.Principal != "alice" || ev.Detail["exit_code"] != "0" {
		t.Fatalf("exec record %+v", ev)
	}

	// A command the deny rules refuse is refused in-band, and recorded.
	ws, err = r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":exec", protoExec)
	if err != nil {
		t.Fatal(err)
	}
	execs := r.fake.Calls("exec")
	_ = ws.WriteMessage(wsconn.Text, []byte(`{"command":"rm -rf /"}`))
	if c, _ := control(t, ws); c.Type != "error" || c.Error.Code != "forbidden" {
		t.Fatalf("refused exec %+v", c)
	}
	if r.fake.Calls("exec") != execs {
		t.Fatal("a refused command ran")
	}
	waitAudit(t, r.cpRig, sb.ID, "exec", controlplane.OutcomeFailed)
	// So is the same command without stdin.
	rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", token: "alice", body: `{"command":"rm -rf /"}`})
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden" {
		t.Fatalf("refused POST exec: %d %s", rec.Code, rec.Body.String())
	}

	// The first message must be the request.
	ws, _ = r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":exec", protoExec)
	_ = ws.WriteMessage(wsconn.Binary, []byte("x"))
	if c, _ := control(t, ws); c.Type != "error" || c.Error.Code != "invalid" {
		t.Fatalf("binary first %+v", c)
	}
}

// A message over the size limit closes the stream with 1009.
func TestSandboxStreamMessageLimit(t *testing.T) {
	r := newWSRig(t, nil, []driver.Capability{driver.CapAttach})
	sb := r.ready(t, "alice")
	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":attach", protoAttach)
	if err != nil {
		t.Fatal(err)
	}
	control(t, ws)
	_ = ws.WriteMessage(wsconn.Binary, make([]byte, streamLimits.MaxMessage+1))
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := ws.ReadMessage()
		if err == nil {
			continue
		}
		if !wsconn.IsClose(err, wsconn.CloseTooBig) {
			t.Fatalf("oversized message: %v", err)
		}
		break
	}
}

func TestSandboxForwardRoundTripAndCloseOnStop(t *testing.T) {
	r := newWSRig(t, nil, []driver.Capability{driver.CapForward})
	sb := r.ready(t, "alice")
	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":forward?port=8080", protoForward)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := control(t, ws); c.Type != "started" {
		t.Fatalf("first message %+v", c)
	}
	_ = ws.WriteMessage(wsconn.Binary, []byte("ping"))
	if got := readUntil(t, ws, "ping"); got != "ping" {
		t.Fatalf("echo %q", got)
	}
	if ev := waitAudit(t, r.cpRig, sb.ID, "forward", controlplane.OutcomeRequested); ev.Detail["port"] != "8080" || ev.Principal != "alice" {
		t.Fatalf("forward request %+v", ev)
	}
	// Stopping the sandbox closes the forward.
	if rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":stop", token: "alice", body: `{"grace_seconds":0}`}); rec.Code != http.StatusAccepted {
		t.Fatalf("stop: %d", rec.Code)
	}
	c, _ := control(t, ws)
	if c.Type != "error" || c.Error.Code != "conflict" {
		t.Fatalf("after stop %+v", c)
	}
	ev := waitAudit(t, r.cpRig, sb.ID, "forward", controlplane.OutcomeFailed)
	if ev.Detail["bytes_in"] != "4" || ev.Detail["bytes_out"] != "4" {
		t.Fatalf("forward record %+v", ev)
	}
	// A bad port is refused before the upgrade.
	r2 := newWSRig(t, nil, []driver.Capability{driver.CapForward})
	sb2 := r2.ready(t, "alice")
	if _, err := r2.dial(t, "alice", "/v1/sandboxes/"+sb2.ID+":forward?port=0", protoForward); handshakeStatus(t, err) != http.StatusBadRequest {
		t.Fatalf("port 0: %v", err)
	}
}

func TestSandboxStreamRateAndSlots(t *testing.T) {
	r := newWSRig(t, map[string]string{controlplane.SetExecPerMin: "1"}, []driver.Capability{driver.CapAttach})
	sb := r.ready(t, "alice")
	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":attach", protoAttach)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if _, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":attach", protoAttach); handshakeStatus(t, err) != http.StatusTooManyRequests {
		t.Fatalf("over the rate: %v", err)
	}
	var l streamSlots
	var releases []func()
	for range maxStreamsPerCaller {
		rel, ok := l.take("alice")
		if !ok {
			t.Fatal("refused under the bound")
		}
		releases = append(releases, rel)
	}
	if _, ok := l.take("alice"); ok {
		t.Fatal("a stream past the per-caller bound")
	}
	if _, ok := l.take("bob"); !ok {
		t.Fatal("another caller was refused")
	}
	releases[0]()
	releases[0]()
	if _, ok := l.take("alice"); !ok {
		t.Fatal("a released slot was not given back")
	}
	if _, ok := l.take("alice"); ok {
		t.Fatal("a release counted twice")
	}
}

// sshClient connects to the sandbox's SSH server through the relay, as
// abhed sandbox connect --stdio does for OpenSSH, trusting the host key
// :ssh names.
func sshClient(t *testing.T, r *wsRig, id string) *ssh.Client {
	t.Helper()
	rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + id + ":ssh", token: "alice"})
	if rec.Code != http.StatusOK {
		t.Fatalf(":ssh: %d %s", rec.Code, rec.Body.String())
	}
	var info struct {
		HostKey, Relay, User, Subprotocol string `json:"-"`
	}
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	info.HostKey, _ = raw["host_key"].(string)
	info.Relay, _ = raw["relay"].(string)
	info.User, _ = raw["user"].(string)
	info.Subprotocol, _ = raw["subprotocol"].(string)
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(info.HostKey))
	if err != nil {
		t.Fatalf("host key %q: %v", info.HostKey, err)
	}
	ws, err := r.dial(t, "alice", info.Relay, info.Subprotocol)
	if err != nil {
		t.Fatal(err)
	}
	cc, chans, reqs, err := ssh.NewClientConn(wsconn.NewStream(ws), "abhed-"+id, &ssh.ClientConfig{User: info.User,
		HostKeyCallback: ssh.FixedHostKey(pub), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c := ssh.NewClient(cc, chans, reqs)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestSandboxSSHThroughTheRelay(t *testing.T) {
	r := newWSRig(t, nil, []driver.Capability{driver.CapAttach, driver.CapForward}, func(o *Options) {
		o.SandboxSSHKey = testHostKey(t)
		o.Config.Permissions.Deny = []string{"bash(rm *)"}
	})
	shellFake(r.cpRig)
	sb := r.ready(t, "alice")
	c := sshClient(t, r, sb.ID)

	// A command, with stdin, and its exit status.
	s, _ := c.NewSession()
	s.Stdin = strings.NewReader("in")
	out, err := s.Output("cat")
	if err != nil || string(out) != "cat\nin" {
		t.Fatalf("exec: %q %v", out, err)
	}
	waitAudit(t, r.cpRig, sb.ID, "exec", controlplane.OutcomeOK)
	// The deny rules hold over SSH.
	s, _ = c.NewSession()
	var errOut bytes.Buffer
	s.Stderr = &errOut
	execs := r.fake.Calls("exec")
	if err := s.Run("rm -rf /"); err == nil || !strings.Contains(errOut.String(), "denied by policy") || r.fake.Calls("exec") != execs {
		t.Fatalf("refused exec: %v %q", err, errOut.String())
	}

	// A shell on a terminal: lines judged, the exit status passed on.
	s, _ = c.NewSession()
	if err := s.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	stdin, _ := s.StdinPipe()
	stdout, _ := s.StdoutPipe()
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(stdout, buf); err != nil || string(buf) != "$ " {
		t.Fatalf("prompt %q %v", buf, err)
	}
	_, _ = stdin.Write([]byte("rm -rf /\r"))
	found := make(chan string, 1)
	go func() {
		got := make([]byte, 0, 256)
		for !bytes.Contains(got, []byte("Denied")) {
			b := make([]byte, 256)
			n, err := stdout.Read(b)
			got = append(got, b[:n]...)
			if err != nil {
				break
			}
		}
		found <- string(got)
	}()
	select {
	case got := <-found:
		if !strings.Contains(got, "Denied") || strings.Contains(got, "rm -rf") {
			t.Fatalf("the refused line: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no refusal on the terminal")
	}
	_, _ = stdin.Write([]byte("exit\r"))
	var exit *ssh.ExitError
	if err := s.Wait(); !errors.As(err, &exit) || exit.ExitStatus() != 7 {
		t.Fatalf("shell end: %v", err)
	}
	if ev := waitAudit(t, r.cpRig, sb.ID, "attach", controlplane.OutcomeOK); ev.Detail["via"] != "ssh" || ev.Detail["exit_code"] != "7" {
		t.Fatalf("ssh shell record %+v", ev)
	}
	waitAudit(t, r.cpRig, sb.ID, "attach.refused", controlplane.OutcomeFailed)

	// A forward to the sandbox's loopback, recorded with its bytes.
	conn, err := c.Dial("tcp", "127.0.0.1:5432")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("hello"))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "he" {
		t.Fatalf("forward %q %v", buf, err)
	}
	_ = conn.Close()
	_ = c.Close()
	ev := waitAudit(t, r.cpRig, sb.ID, "ssh", controlplane.OutcomeOK)
	if ev.Principal != "alice" || ev.Detail["forwards"] != "1" {
		t.Fatalf("ssh record %+v", ev)
	}
	if ev := waitAudit(t, r.cpRig, sb.ID, "forward", controlplane.OutcomeOK); ev.Detail["via"] != "ssh" || ev.Detail["port"] != "5432" {
		t.Fatalf("ssh forward record %+v", ev)
	}
}

// revocable is a verifier whose sign-ins can be withdrawn.
type revocable struct {
	mu      sync.Mutex
	revoked map[string]bool
	inner   tokens
}

func (v *revocable) Verify(ctx context.Context, token string) (*auth.Identity, error) {
	v.mu.Lock()
	gone := v.revoked[token]
	v.mu.Unlock()
	if gone {
		return nil, errors.New("signed out")
	}
	return v.inner.Verify(ctx, token)
}

// An open stream ends once its caller's sign-in does.
func TestSandboxStreamEndsWithTheSignIn(t *testing.T) {
	v := &revocable{revoked: map[string]bool{}}
	r := newWSRig(t, nil, []driver.Capability{driver.CapAttach}, func(o *Options) {
		v.inner = o.Auth.Verifier.(tokens)
		o.Auth.Verifier = v
	})
	sb := r.ready(t, "alice")
	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":attach", protoAttach)
	if err != nil {
		t.Fatal(err)
	}
	control(t, ws)
	v.mu.Lock()
	v.revoked["alice"] = true
	v.mu.Unlock()
	r.srv.RecheckStreams()
	c, _ := control(t, ws)
	if c.Type != "error" || c.Error.Code != "forbidden" {
		t.Fatalf("after sign-out %+v", c)
	}
	waitAudit(t, r.cpRig, sb.ID, "attach", controlplane.OutcomeFailed)
}

// Each SSH session counts against the caller's exec rate, as the verb would:
// here :ssh, the relay and one command spend it.
func TestSandboxSSHSessionsAreRateLimited(t *testing.T) {
	r := newWSRig(t, map[string]string{controlplane.SetExecPerMin: "3"}, []driver.Capability{driver.CapAttach},
		func(o *Options) { o.SandboxSSHKey = testHostKey(t) })
	sb := r.ready(t, "alice")
	c := sshClient(t, r, sb.ID)
	s, _ := c.NewSession()
	if out, err := s.Output("true"); err != nil {
		t.Fatalf("first: %q %v", out, err)
	}
	s, _ = c.NewSession()
	var errOut bytes.Buffer
	s.Stderr = &errOut
	execs := r.fake.Calls("exec")
	if err := s.Run("true"); err == nil || !strings.Contains(errOut.String(), "too many") || r.fake.Calls("exec") != execs {
		t.Fatalf("over the rate: %v %q", err, errOut.String())
	}
}
