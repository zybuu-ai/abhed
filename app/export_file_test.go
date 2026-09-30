package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openExport writes a new file or a plain one, and nothing through a link
// or over a second name, wherever it points.
func TestOpenExportRefusesLinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	_ = os.WriteFile(target, []byte("keep"), 0o600)
	link := filepath.Join(dir, "link.txt")
	_ = os.Symlink(target, link)
	hard := filepath.Join(dir, "hard.txt")
	_ = os.Link(target, hard)
	for p, want := range map[string]string{link: "not a plain file", hard: "another name", target: "another name"} {
		if f, err := openExport(p); err == nil || !strings.Contains(err.Error(), want) {
			if f != nil {
				_ = f.Close()
			}
			t.Errorf("%s: %v, want %q", filepath.Base(p), err, want)
		}
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatal("the target was written")
	}
	plain := filepath.Join(dir, "plain.txt")
	_ = os.WriteFile(plain, []byte("old"), 0o644)
	for _, p := range []string{plain, filepath.Join(dir, "new.txt")} {
		f, err := openExport(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		_, _ = f.WriteString("new")
		_ = f.Close()
		if b, _ := os.ReadFile(p); string(b) != "new" {
			t.Fatalf("%s = %q", p, b)
		}
	}
}
