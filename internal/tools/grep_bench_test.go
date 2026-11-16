package tools

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// BenchmarkGrep searches the folder ABHED_BENCH_GREP_DIR with the Go search
// and with ripgrep (ABHED_TEST_RIPGREP, or one on PATH) narrowing it.
func BenchmarkGrep(b *testing.B) {
	dir := os.Getenv("ABHED_BENCH_GREP_DIR")
	if dir == "" {
		b.Skip("set ABHED_BENCH_GREP_DIR to a large checkout")
	}
	s, err := NewSession(dir)
	if err != nil {
		b.Fatal(err)
	}
	for _, pattern := range []string{"Describe what to build", "Abhed"} {
		raw, _ := json.Marshal(grepArgs{Pattern: pattern})
		for _, path := range []string{"go", "ripgrep"} {
			rg := ""
			if path == "ripgrep" {
				if rg = realRipgrep(); rg == "" {
					continue
				}
			}
			b.Run(pattern+"/"+path, func(b *testing.B) {
				was := ripgrep
				ripgrep = rg
				defer func() { ripgrep = was }()
				for b.Loop() {
					if res := (Grep{}).Run(context.Background(), s, raw); res.IsError {
						b.Fatal(res.Content)
					}
				}
			})
		}
	}
}
