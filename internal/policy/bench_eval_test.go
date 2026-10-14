package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkEvaluateWrite(b *testing.B) {
	ws := b.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src/pkg/deep"), 0o755); err != nil {
		b.Fatal(err)
	}
	e := New(ModeDefault)
	e.Roots = func() []string { return []string{ws, ws + "/src"} }
	if err := e.AddDeny("write(**/vault/**)", "write(**/.env)"); err != nil {
		b.Fatal(err)
	}
	a := args(map[string]string{"path": filepath.Join(ws, "src/pkg/deep/file.go")})
	b.ResetTimer()
	for range b.N {
		e.Evaluate("write", true, a)
	}
}

func BenchmarkEvaluateWriteAllowOnly(b *testing.B) {
	ws := b.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src/pkg/deep"), 0o755); err != nil {
		b.Fatal(err)
	}
	e := New(ModeDefault)
	e.Roots = func() []string { return []string{ws} }
	if err := e.AddAllow("write(src/**)"); err != nil {
		b.Fatal(err)
	}
	a := args(map[string]string{"path": filepath.Join(ws, "src/pkg/deep/file.go")})
	b.ResetTimer()
	for range b.N {
		e.Evaluate("write", true, a)
	}
}

// BenchmarkEvaluateBraces is a 55 KB command of brace sequences under a deny
// rule; past the brace budget it asks, in tens of milliseconds.
func BenchmarkEvaluateBraces(b *testing.B) {
	e := New(ModeBypass)
	if err := e.AddDeny("bash(curl *)"); err != nil {
		b.Fatal(err)
	}
	a := args(map[string]string{"command": strings.Repeat("echo {1..1000}\n", 3600)})
	b.ResetTimer()
	for range b.N {
		e.Evaluate("bash", true, a)
	}
}
