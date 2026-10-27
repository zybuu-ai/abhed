package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/secretfiles"
)

// A server that dies at the start says why on stderr; the error carries the
// end of it, not only a closed pipe.
func TestStartFailureCarriesStderr(t *testing.T) {
	tr, err := NewStdioTransport(context.Background(), "/bin/sh", []string{"-c", "echo 'line one' >&2; echo 'PermissionError: cannot read /x/server.py' >&2; exit 3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient("tickets", tr)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = c.Initialize(ctx)
	if err == nil || !strings.Contains(err.Error(), "PermissionError: cannot read /x/server.py") || !strings.Contains(err.Error(), "the server wrote") {
		t.Fatalf("error %v", err)
	}
}

// The tail is bounded and printable.
func TestStderrTailBounded(t *testing.T) {
	b := &tailBuffer{}
	for range 2000 {
		_, _ = b.Write([]byte("noise \x1b[31mred\x1b[0m line\n"))
	}
	tr := &StdioTransport{stderr: b}
	tail := tr.StderrTail(0)
	if len(tail) > 1100 || strings.ContainsRune(tail, 0x1b) || !strings.Contains(tail, "line") {
		t.Fatalf("tail %q", tail)
	}
}

// What the server wrote is redacted before it is told: its own environment's
// values, a token passed as a flag, and anything shaped like a key.
func TestStderrTailRedacted(t *testing.T) {
	script := `echo "auth failed for token=abcd1234secretvalue" >&2; echo "flag tok s3cr3t-flag-value" >&2; ` +
		`echo "key ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789 rejected" >&2; echo "PATH is $PATH" >&2; exit 1`
	tr, err := NewStdioTransport(context.Background(), "/bin/sh", []string{"-c", script, "sh", "--api-token", "s3cr3t-flag-value"},
		[]string{"PATH=/usr/bin:/bin", "TICKETS_TOKEN=abcd1234secretvalue"})
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient("tickets", tr)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = c.Initialize(ctx)
	if err == nil {
		t.Fatal("initialized")
	}
	msg := err.Error()
	for _, leak := range []string{"abcd1234secretvalue", "s3cr3t-flag-value", "ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789"} {
		if strings.Contains(msg, leak) {
			t.Errorf("%q reached the error: %s", leak, msg)
		}
	}
	if !strings.Contains(msg, "[secret]") || !strings.Contains(msg, "PATH is /usr/bin:/bin") || !strings.Contains(msg, "auth failed") {
		t.Errorf("error %s", msg)
	}
}

// A confined server's environment is not its launcher's: a container
// engine's command carries the host's, with the server's in a file. Its
// stderr is still redacted against the server's own, and the host's own
// values are left as written.
func TestConfinedServerStderrRedacted(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "server.env")
	g := NewGateway()
	g.Confine = func(ctx context.Context, _ string, _, env []string, _ func(string, map[string]any) error) (*exec.Cmd, error) {
		var b strings.Builder
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			fmt.Fprintf(&b, "%s='%s'\n", k, v)
		}
		if err := os.WriteFile(envFile, []byte(b.String()), 0o600); err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `. "$0"; echo "auth failed for $TICKETS_TOKEN at $HOSTVALUE" >&2; exit 1`, envFile)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOSTVALUE=/opt/host/value"}
		return cmd, nil
	}
	errs := g.Connect(context.Background(), []ServerConfig{{Name: "tickets", Command: "/bin/true", Enabled: true,
		Env: []string{"TICKETS_TOKEN=opaqueTicketsValue42"}}})
	if len(errs) != 1 {
		t.Fatalf("errors %v", errs)
	}
	msg := errs[0].Error()
	if strings.Contains(msg, "opaqueTicketsValue42") || !strings.Contains(msg, "auth failed for [secret] at /opt/host/value") {
		t.Fatalf("error %s", msg)
	}
}

// Secrets given as arguments: -e NAME=VALUE, --env, a header and NAME=VALUE.
func TestStderrRedactsSecretArguments(t *testing.T) {
	tr := &StdioTransport{argv: []string{"docker", "run", "-e", "API_KEY=opaqueKeyValue1", "--env=DB_PASSWORD=opaquePass22",
		"--header", "Authorization: Bearer opaqueBearer333", "X_TOKEN=opaqueToken4444", "-e", "MODE=plainvalue"}}
	out := tr.redact("got opaqueKeyValue1 opaquePass22 opaqueBearer333 opaqueToken4444 plainvalue")
	if strings.Contains(out, "opaque") || !strings.Contains(out, "plainvalue") {
		t.Fatalf("redacted %q", out)
	}
	if got, _ := secretfiles.RedactContent("Authorization: Bearer abc123def456ghi"); strings.Contains(got, "abc123def456ghi") {
		t.Fatalf("bearer shape %q", got)
	}
	if got, _ := secretfiles.RedactContent("uses Bearer authentication"); got != "uses Bearer authentication" {
		t.Fatalf("a word was taken for a token: %q", got)
	}
}
