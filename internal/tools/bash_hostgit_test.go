package tools

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// A line run through host git as the person is given no vault secret: it
// needs none, and its environment is the person's own.
func TestBashGivesHostGitNoSecrets(t *testing.T) {
	old := HostGitLine
	HostGitLine = func(line string) bool { return strings.HasPrefix(line, "git fetch") }
	t.Cleanup(func() { HostGitLine = old })
	asked := false
	b := Bash{
		Sandbox: func(ctx context.Context, _, _ string) *exec.Cmd { return exec.CommandContext(ctx, "true") },
		HostAsk: func(string, string, string) (string, string, string, error) { return "", "", "", nil },
		Secrets: func([]string) ([]string, error) { asked = true; return []string{"TOKEN=s3cret"}, nil },
	}
	if _, err := b.command(context.Background(), t.TempDir(), bashArgs{Command: "git fetch", Secrets: []string{"TOKEN"}}); err == nil ||
		!strings.Contains(err.Error(), "not given to git run on the host") || asked {
		t.Fatalf("a brokered line took secrets: %v (vault asked %v)", err, asked)
	}
	cmd, err := b.command(context.Background(), t.TempDir(), bashArgs{Command: "make deploy", Secrets: []string{"TOKEN"}})
	if err != nil || !strings.Contains(strings.Join(cmd.Env, " "), "TOKEN=s3cret") {
		t.Fatalf("another line lost its secret: %v", err)
	}
}
