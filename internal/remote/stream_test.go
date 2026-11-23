package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Stream carries the caller's stdin and stdout to the command, and a host
// whose key differs from the one recorded is refused before anything runs.
func TestStreamAndHostKeyMismatch(t *testing.T) {
	srv := startIOServer(t, "hunter2")
	t.Setenv("TEST_SSH_PW", "hunter2")
	h, err := NewHost(HostConfig{Name: "vm1", Addr: srv.addr, User: "tester", PasswordEnv: "TEST_SSH_PW", KnownHostsFile: knownHostsFor(t, srv.testServer)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	var out, errw bytes.Buffer
	code, err := h.Stream(context.Background(), "echo-stdin", strings.NewReader("over the stream"), &out, &errw)
	if err != nil || code != 0 || out.String() != "over the stream" {
		t.Fatalf("stream: %d %v %q %q", code, err, out.String(), errw.String())
	}

	// The command's end ends the stream, whatever the caller's stdin does.
	pr, pw := io.Pipe()
	defer pw.Close()
	doneCh := make(chan struct{})
	go func() {
		_, _ = h.Stream(context.Background(), "ran-and-exited", pr, &out, &errw)
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream waited for stdin after the command ended")
	}

	// Another key recorded for the same address: refused, nothing run.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(other)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(knownhosts.Line([]string{knownhosts.Normalize(srv.addr)}, signer.PublicKey())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad, _ := NewHost(HostConfig{Name: "vm1", Addr: srv.addr, User: "tester", PasswordEnv: "TEST_SSH_PW", KnownHostsFile: kh})
	before, _ := srv.ran()
	if _, err := bad.Stream(context.Background(), "echo-stdin", strings.NewReader("x"), &out, &errw); err == nil || !strings.Contains(err.Error(), "does not match the one known_hosts") {
		t.Fatalf("a mismatched host key was accepted: %v", err)
	}
	if after, _ := srv.ran(); len(after) != len(before) {
		t.Fatal("a command ran on a host whose key did not match")
	}
	// The algorithms asked for are the recorded key's.
	cb, _ := knownhosts.New(kh)
	if algs := knownAlgorithms(cb, srv.addr); len(algs) == 0 || algs[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("algorithms %v", algs)
	}
}
