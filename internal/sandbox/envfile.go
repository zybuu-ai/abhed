package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// EnvFile writes env to a file only this user can read, in a folder of its
// own, for an engine CLI's --env-file: values passed by name would sit in
// the CLI's own environment, where its client to the engine reads proxies.
func EnvFile(env []string) (string, func(), error) {
	var b strings.Builder
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.TrimSpace(k) != k || k[0] == '#' || strings.ContainsAny(kv, "\n\r\x00") {
			return "", nil, errors.New("an environment entry an env file cannot carry")
		}
		b.WriteString(kv + "\n")
	}
	dir, err := os.MkdirTemp("", "abhed-env-")
	if err != nil {
		return "", nil, err
	}
	remove := func() { _ = os.RemoveAll(dir) }
	p := filepath.Join(dir, "env")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		remove()
		return "", nil, err
	}
	return p, remove, nil
}
