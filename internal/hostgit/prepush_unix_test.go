//go:build unix

package hostgit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The relay hands the hook the remote's URL without its user and password:
// a token the person's insteadOf puts there never reaches the sandbox.
func TestPrePushRedactsTheURL(t *testing.T) {
	dir := t.TempDir()
	old := relayParent
	relayParent = func() (string, error) { return dir, nil }
	t.Cleanup(func() { relayParent = old })
	var mu sync.Mutex
	var lines []string
	b := &HostBroker{command: func(ctx context.Context, _, line string) *exec.Cmd {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
		return exec.CommandContext(ctx, "true")
	}}
	r, err := b.prePushRelay(context.Background(), plan{root: dir, hooks: "/hooks"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	req, err := os.OpenFile(filepath.Join(r.dir, "req"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = req.WriteString("origin\nhttps://oauth2:HOOK-TOKEN-5@git.example/a.git\nrefs/heads/main 1 refs/heads/main 0\n" + relayEnd + "\n")
	_ = req.Close()
	resp, err := os.Open(filepath.Join(r.dir, "resp"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, _ = resp.Read(make([]byte, 8))
	_ = resp.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || strings.Contains(lines[0], "HOOK-TOKEN-5") || !strings.Contains(lines[0], "'https://git.example/a.git'") ||
		!strings.Contains(lines[0], "refs/heads/main 1 refs/heads/main 0") {
		t.Fatalf("the sandbox was given %q", lines)
	}
}
