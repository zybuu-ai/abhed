package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

const keyPEM = "-----BEGIN PRIVATE KEY-----\nTUFURVJJQUwtTkVWRVItU0hPV04=\n-----END PRIVATE KEY-----\n"

// sftpHost is a host served by OpenSSH's own sftp-server, and a folder on
// "the host" to transfer into. The test is skipped where no sftp-server is
// installed, and says so.
func sftpHost(t *testing.T) (*Registry, string) {
	t.Helper()
	if sftpServerBinary() == "" {
		t.Skipf("no OpenSSH sftp-server at %v", sftpServerPaths)
	}
	_, reg := ioHost(t)
	remote, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return reg, remote
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func call(t *testing.T, tool tools.Tool, sess *tools.Session, args map[string]any) tools.Result {
	t.Helper()
	raw, _ := json.Marshal(args)
	if pc, ok := tool.(tools.Prechecker); ok {
		if err := pc.Precheck(sess, raw); err != nil {
			return tools.Result{Content: "precheck: " + err.Error(), IsError: true}
		}
	}
	return tool.Run(context.Background(), sess, raw)
}

func TestPutAFileAndASecret(t *testing.T) {
	reg, remote := sftpHost(t)
	sess := newSession(t)
	crt := filepath.Join(sess.Root, "site.crt")
	body := []byte("-----BEGIN CERTIFICATE-----\nQ0VSVA==\n-----END CERTIFICATE-----\n")
	if err := os.WriteFile(crt, body, 0o600); err != nil {
		t.Fatal(err)
	}
	put := PutTool{R: reg, Secret: stored(map[string]string{"SITE_KEY": keyPEM})}

	dst := filepath.Join(remote, "site.crt")
	if err := os.WriteFile(dst, []byte("old certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := call(t, put, sess, map[string]any{"host": "vm1", "remote_path": dst, "file": crt})
	if res.IsError || !strings.Contains(res.Content, shaHex(body)) || !strings.Contains(res.Content, "mode 0644") {
		t.Fatalf("put file: %s", res.Content)
	}
	if got, _ := os.ReadFile(dst); string(got) != string(body) {
		t.Fatalf("the host holds %q", got)
	}

	keyDst := filepath.Join(remote, "site.key")
	res = call(t, put, sess, map[string]any{"host": "vm1", "remote_path": keyDst, "secret": "SITE_KEY"})
	if res.IsError || strings.Contains(res.Content, "TUFURVJJQUw") || strings.Contains(res.Content, shaHex([]byte(keyPEM))) ||
		!strings.Contains(res.Content, "fingerprint "+secrets.Fingerprint([]byte(keyPEM))) {
		t.Fatalf("put secret: %s", res.Content)
	}
	info, err := os.Stat(keyDst)
	if got, _ := os.ReadFile(keyDst); err != nil || string(got) != keyPEM || info.Mode().Perm() != 0o600 {
		t.Fatalf("the host holds %q, mode %v", got, info.Mode())
	}
	// Each file went in whole, through a file beside it that is gone.
	entries, _ := os.ReadDir(remote)
	if len(entries) != 2 {
		t.Fatalf("left on the host: %v", entries)
	}
}

func TestGetIntoTheWorkspace(t *testing.T) {
	reg, remote := sftpHost(t)
	sess := newSession(t)
	src := filepath.Join(remote, "nginx.conf")
	body := []byte(strings.Repeat("server { listen 443; }\n", 5000)) // several SFTP reads
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	get := GetTool{R: reg}
	dst := filepath.Join(sess.Root, "fetched", "nginx.conf")
	res := call(t, get, sess, map[string]any{"host": "vm1", "remote_path": src, "file": dst})
	if res.IsError || !strings.Contains(res.Content, shaHex(body)) || strings.Contains(res.Content, "listen 443") {
		t.Fatalf("get: %s", res.Content)
	}
	if got, _ := os.ReadFile(dst); string(got) != string(body) {
		t.Fatalf("fetched %d bytes, want %d", len(got), len(body))
	}
	// An existing file the session has not read is not replaced.
	res = call(t, get, sess, map[string]any{"host": "vm1", "remote_path": src, "file": dst})
	if !res.IsError || !strings.Contains(res.Content, "has not been read") {
		t.Fatalf("replaced an unread file: %s", res.Content)
	}
	// Too large is refused before it is read; missing says so.
	small := GetTool{R: reg, Max: 1024}
	res = call(t, small, sess, map[string]any{"host": "vm1", "remote_path": src, "file": filepath.Join(sess.Root, "big")})
	if !res.IsError || !strings.Contains(res.Content, "more than the 1024") {
		t.Fatalf("over the bound: %s", res.Content)
	}
	res = call(t, get, sess, map[string]any{"host": "vm1", "remote_path": filepath.Join(remote, "nope"), "file": filepath.Join(sess.Root, "n")})
	if !res.IsError || !strings.Contains(res.Content, "does not exist") {
		t.Fatalf("missing: %s", res.Content)
	}
}

// A call that cannot succeed is refused before anyone is asked.
func TestTransferPrechecks(t *testing.T) {
	_, reg := ioHost(t)
	sess := newSession(t)
	put := PutTool{R: reg, Secret: stored(map[string]string{"K": "v"}), Max: 8}
	big := filepath.Join(sess.Root, "big")
	_ = os.WriteFile(big, []byte("more than eight bytes"), 0o600)
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"host": "vm1", "remote_path": "etc/x", "secret": "K"}, "must be absolute"},
		{map[string]any{"host": "vm1", "remote_path": "/etc/../x", "secret": "K"}, "must be clean"},
		{map[string]any{"host": "vm1", "remote_path": "/etc/x", "secret": "K", "file": big}, "exactly one of"},
		{map[string]any{"host": "vm9", "remote_path": "/etc/x", "secret": "K"}, `no host named "vm9"`},
		{map[string]any{"host": "vm1", "remote_path": "/etc/x", "file": big}, "more than the 8"},
		{map[string]any{"host": "vm1", "remote_path": "/etc/x", "file": "/etc/passwd"}, "outside"},
		{map[string]any{"host": "vm1", "remote_path": "/etc/x", "secret": "K", "mode": "999"}, "not octal"},
	} {
		raw, _ := json.Marshal(tc.args)
		err := put.Precheck(sess, raw)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v, want %q", tc.args, err, tc.want)
		}
	}
	raw, _ := json.Marshal(map[string]any{"host": "vm1", "remote_path": "/etc/x", "file": "/tmp/elsewhere"})
	if err := (GetTool{R: reg}).Precheck(sess, raw); err == nil {
		t.Error("ssh_get would write outside the workspace")
	}
}

func TestTransferSubjectIsHostAndPath(t *testing.T) {
	got, ok := PutTool{}.PolicySubject(json.RawMessage(`{"host":"web1","remote_path":"/etc/nginx/site.crt","file":"/w/x"}`))
	if !ok || got != "web1:/etc/nginx/site.crt" {
		t.Fatalf("subject %q", got)
	}
}

type oneTool struct {
	name, args string
	n          int
}

func (*oneTool) Name() string                           { return "one" }
func (*oneTool) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (*oneTool) CountTokens(model.Request) (int, error) { return 0, nil }
func (o *oneTool) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 2)
	if o.n == 0 {
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &model.ToolCall{ID: "c1", Name: o.name, Args: json.RawMessage(o.args)}}
	} else {
		ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	}
	o.n++
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
	close(ch)
	return ch, nil
}

// Through the loop: a secret needs its rule, a file is judged as a read or
// write of it, the approval names where it goes, and the record never holds
// the content.
func TestTransfersThroughTheLoop(t *testing.T) {
	reg, remote := sftpHost(t)
	run := func(pol *policy.Engine, sess *tools.Session, name, args string) (*seen, []agent.Event) {
		ap := &seen{}
		store := agent.NewMemStore()
		reg := tools.NewRegistry(PutTool{R: reg, Secret: stored(map[string]string{"SITE_KEY": keyPEM})}, GetTool{R: reg})
		l := agent.NewLoop(&oneTool{name: name, args: args}, reg, pol, ap, sess, agent.NewRecorder(store, "s", ""), agent.DefaultConfig())
		if _, err := l.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		evs, _ := store.Events("s")
		return ap, evs
	}
	marshal := func(m map[string]any) string { b, _ := json.Marshal(m); return string(b) }
	sess := newSession(t)
	keyDst := filepath.Join(remote, "site.key")
	putKey := marshal(map[string]any{"host": "vm1", "remote_path": keyDst, "secret": "SITE_KEY"})

	if ap, _ := run(policy.New(policy.ModeDefault), sess, "ssh_put", putKey); len(ap.reasons) != 0 {
		t.Fatalf("asked without secret(SITE_KEY): %v", ap.reasons)
	}
	if _, err := os.Stat(keyDst); err == nil {
		t.Fatal("sent without secret(SITE_KEY)")
	}
	pol := policy.New(policy.ModeDefault)
	_ = pol.AddAllow("secret(SITE_KEY)")
	ap, evs := run(pol, sess, "ssh_put", putKey)
	if len(ap.reasons) != 1 || !strings.Contains(ap.reasons[0], "writes "+keyDst+" on tester@") ||
		!strings.Contains(ap.reasons[0], "from stored secret SITE_KEY, mode 0600") {
		t.Fatalf("asked %v", ap.reasons)
	}
	for _, ev := range evs {
		if strings.Contains(string(ev.Payload), "TUFURVJJQUw") {
			t.Errorf("%s holds the key: %s", ev.Type, ev.Payload)
		}
	}
	if got, _ := os.ReadFile(keyDst); string(got) != keyPEM {
		t.Fatalf("the host holds %q", got)
	}

	// Rules on files hold whichever tool reaches them.
	secretFile := filepath.Join(sess.Root, "id.key")
	_ = os.WriteFile(secretFile, []byte(keyPEM), 0o600)
	pol = policy.New(policy.ModeDefault)
	_ = pol.AddDeny("read(*.key)", "write(*.pem)")
	if ap, _ := run(pol, sess, "ssh_put", marshal(map[string]any{"host": "vm1", "remote_path": filepath.Join(remote, "k"), "file": secretFile})); len(ap.reasons) != 0 {
		t.Fatalf("a denied file was offered for sending: %v", ap.reasons)
	}
	if _, err := os.Stat(filepath.Join(remote, "k")); err == nil {
		t.Fatal("sent a file read rules deny")
	}
	if ap, _ := run(pol, sess, "ssh_get", marshal(map[string]any{"host": "vm1", "remote_path": keyDst, "file": filepath.Join(sess.Root, "x.pem")})); len(ap.reasons) != 0 {
		t.Fatalf("a denied write was offered: %v", ap.reasons)
	}
	if _, err := os.Stat(filepath.Join(sess.Root, "x.pem")); err == nil {
		t.Fatal("wrote a file write rules deny")
	}
	// An allow rule on the host and path lets a fetch through without asking.
	pol = policy.New(policy.ModeDefault)
	_ = pol.AddAllow("ssh_get(vm1:" + remote + "/*)")
	got := filepath.Join(sess.Root, "got.key")
	if ap, _ := run(pol, sess, "ssh_get", marshal(map[string]any{"host": "vm1", "remote_path": keyDst, "file": got})); len(ap.reasons) != 0 {
		t.Fatalf("asked despite the allow rule: %v", ap.reasons)
	}
	if b, _ := os.ReadFile(got); string(b) != keyPEM {
		t.Fatalf("fetched %q", b)
	}
	// An ask rule on the file it writes makes that allowed fetch ask.
	_ = pol.AddAsk("write(*.conf)")
	if ap, _ := run(pol, sess, "ssh_get", marshal(map[string]any{"host": "vm1", "remote_path": keyDst, "file": filepath.Join(sess.Root, "a.conf")})); len(ap.reasons) != 1 ||
		!strings.Contains(ap.reasons[0], "a.conf it would write") {
		t.Fatalf("an ask rule on the file did not ask: %v", ap.reasons)
	}
}

// A host that stops answering mid-transfer does not hold the call: the
// transfer ends with its context, and transferTimeout bounds a tool call.
func TestStalledTransferEndsWithItsContext(t *testing.T) {
	defer func(g, tt time.Duration) { sftpGrace, transferTimeout = g, tt }(sftpGrace, transferTimeout)
	sftpGrace = 200 * time.Millisecond
	for _, at := range []string{"init", "read"} {
		t.Run(at, func(t *testing.T) {
			srv, reg := ioHost(t)
			srv.mu.Lock()
			srv.stall = at
			srv.mu.Unlock()
			h, _ := reg.get("vm1")
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := h.Get(ctx, "/etc/x", 1<<30)
			if err == nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("get from a stalled host: %v", err)
			}
			if took := time.Since(start); took > 3*time.Second {
				t.Fatalf("a stalled get took %s", took)
			}
			// The tool's own bound holds without any deadline from the caller.
			transferTimeout = 300 * time.Millisecond
			sess := newSession(t)
			start = time.Now()
			res := call(t, GetTool{R: reg}, sess, map[string]any{"host": "vm1", "remote_path": "/etc/x", "file": filepath.Join(sess.Root, "x")})
			if !res.IsError || !strings.Contains(res.Content, "stopped") || time.Since(start) > 3*time.Second {
				t.Fatalf("tool call on a stalled host after %s: %s", time.Since(start), res.Content)
			}
			res = call(t, PutTool{R: reg, Secret: stored(map[string]string{"K": "v"})}, sess, map[string]any{"host": "vm1", "remote_path": "/etc/x", "secret": "K"})
			if !res.IsError || time.Since(start) > 6*time.Second {
				t.Fatalf("put on a stalled host: %s", res.Content)
			}
		})
	}
}

// A stopped transfer closes the connection it used: one the host dialled
// since, for another call, stays open.
func TestStalledTransferDropsOnlyItsOwnConnection(t *testing.T) {
	_, reg := ioHost(t)
	h, _ := reg.get("vm1")
	used, err := h.connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	fresh, err := h.connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h.drop(used)
	h.mu.Lock()
	kept := h.client
	h.mu.Unlock()
	if kept != fresh {
		t.Fatal("dropping the stopped transfer's connection dropped the one dialled since")
	}
	if _, _, err := fresh.SendRequest("keepalive@abhed", true, nil); err != nil && strings.Contains(err.Error(), "closed") {
		t.Fatalf("the fresh connection was closed: %v", err)
	}
	h.drop(fresh)
	h.mu.Lock()
	gone := h.client == nil
	h.mu.Unlock()
	if !gone {
		t.Fatal("dropping the current connection left it in place")
	}
}
