package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Guard's refusal stops edit and write before they touch the file.
func TestGuardRefusesEditAndWrite(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.Root, "kept.txt")
	if err := os.WriteFile(p, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.MarkRead(p, "mine")
	s.Guard = func(path string, _ []string) error {
		if path == p {
			return errors.New("kept by the guard")
		}
		return nil
	}
	w, _ := json.Marshal(map[string]string{"path": p, "content": "theirs"})
	e, _ := json.Marshal(map[string]string{"path": p, "old_string": "mine", "new_string": "theirs"})
	for name, res := range map[string]Result{"write": (Write{}).Run(context.Background(), s, w), "edit": (Edit{}).Run(context.Background(), s, e)} {
		if !res.IsError || !strings.Contains(res.Content, "kept by the guard") {
			t.Fatalf("%s: %+v", name, res)
		}
	}
	if b, _ := os.ReadFile(p); string(b) != "mine" {
		t.Fatalf("the file changed: %q", b)
	}
	// A fork keeps the guard.
	if res := (Write{}).Run(context.Background(), s.Fork(), w); !res.IsError || !strings.Contains(res.Content, "kept by the guard") {
		t.Fatalf("a fork: %+v", res)
	}
	other, _ := json.Marshal(map[string]string{"path": filepath.Join(s.Root, "new.txt"), "content": "x"})
	if res := (Write{}).Run(context.Background(), s, other); res.IsError {
		t.Fatalf("a file the guard allows: %+v", res)
	}
}
