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
