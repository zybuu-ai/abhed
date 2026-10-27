package mcp

import (
	"context"
	"strings"
	"testing"
	"time"
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
