package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file tools never write into a .git folder or a .git file, in any case
// or through a link: a hook or config line there runs a program at the next
// git command. An ordinary file is written as before.
func TestFileToolsRefuseGitsOwnFolder(t *testing.T) {
	s, err := NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".git/hooks", "sub"} {
		if err := os.MkdirAll(filepath.Join(s.Root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(s.Root, ".git", "config")
	if err := os.WriteFile(cfg, []byte("[core]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.MarkRead(cfg, "[core]\n")
	if err := os.Symlink(filepath.Join(s.Root, ".git", "hooks"), filepath.Join(s.Root, "h")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".git/hooks/pre-commit", ".GIT/hooks/post-checkout", "sub/.git", "h/pre-push", ".git/config"} {
		path := filepath.Join(s.Root, filepath.FromSlash(p))
		w, _ := json.Marshal(map[string]string{"path": path, "content": "#!/bin/sh\ntouch pwned\n"})
		if res := (Write{}).Run(context.Background(), s, w); !res.IsError || !strings.Contains(res.Content, "git's own folder") {
			t.Errorf("write %s: %+v", p, res)
		}
		if err := (Write{}).Precheck(s, w); err == nil {
			t.Errorf("precheck let write %s be asked", p)
		}
	}
	e, _ := json.Marshal(map[string]string{"path": cfg, "old_string": "[core]", "new_string": "[core]\n\thooksPath = /tmp"})
	if res := (Edit{}).Run(context.Background(), s, e); !res.IsError {
		t.Errorf("edit .git/config: %+v", res)
	}
	if data, _ := os.ReadFile(cfg); string(data) != "[core]\n" {
		t.Fatalf("config changed: %q", data)
	}
	for _, p := range []string{"pre-commit", "sub/.git", ".git/hooks/pre-commit"} {
		if _, err := os.Lstat(filepath.Join(s.Root, filepath.FromSlash(p))); err == nil && p != "pre-commit" {
			t.Errorf("%s was written", p)
		}
	}
	ok, _ := json.Marshal(map[string]string{"path": filepath.Join(s.Root, "sub", "notes.gitignore"), "content": "x"})
	if res := (Write{}).Run(context.Background(), s, ok); res.IsError {
		t.Fatalf("an ordinary file was refused: %s", res.Content)
	}
}
