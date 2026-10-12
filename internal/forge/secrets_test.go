package forge

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/secrets"
)

// A push is refused when the change holds a stored value, in a file's text,
// encoded, in a file's name or in a commit message; a clean change is not.
func TestPushRefusesAChangeHoldingASecret(t *testing.T) {
	const v = "ghp_0123456789abcdefABCD"
	cases := map[string]func(t *testing.T, dir string){
		"text": func(t *testing.T, dir string) { write(t, filepath.Join(dir, "cfg.txt"), "token="+v+"\n") },
		"base64": func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "cfg.txt"), base64.StdEncoding.EncodeToString([]byte("u:"+v))+"\n")
		},
		"name": func(t *testing.T, dir string) { write(t, filepath.Join(dir, v+".txt"), "x\n") },
		"message": func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "ok.txt"), "x\n")
			run(t, dir, "add", "-A")
			run(t, dir, "commit", "-q", "-m", "uses "+v)
		},
		"clean": func(t *testing.T, dir string) { write(t, filepath.Join(dir, "ok.txt"), "x\n") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			repo := gitRepo(t)
			vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
			if err := vault.Set("GITHUB_TOKEN", v); err != nil {
				t.Fatal(err)
			}
			w, err := Begin(context.Background(), repo, Issue{Ref: Ref{Number: 9}, Title: "x"}, "")
			if err != nil {
				t.Fatal(err)
			}
			defer w.Cleanup(context.Background(), repo)
			change(t, w.Dir)
			if _, err := w.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			w.Secrets = vault.Redactor()
			ref := Ref{Scheme: "https", Host: "127.0.0.1:1", Owner: "o", Repo: "r", Number: 9}
			err = w.Push(context.Background(), repo, ref, "Basic x", "")
			if got := errors.Is(err, ErrSecretInChange); got != (name != "clean") {
				t.Fatalf("push: %v", err)
			}
		})
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
