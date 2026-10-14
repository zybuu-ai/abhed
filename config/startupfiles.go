package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zybuu-ai/abhed/internal/nlink"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// TrustedStartupFile is a shell start-up file the person trusted as their own
// with abhed trust startup-files, by its content.
type TrustedStartupFile struct {
	Path   string    `json:"path"`
	Size   int64     `json:"size"`
	SHA256 string    `json:"sha256"`
	At     time.Time `json:"at"`
}

type startupFiles struct {
	Version int                  `json:"version"`
	Files   []TrustedStartupFile `json:"files"`
}

// StartupFilesPath is ~/.abhed/startup-files.json, the person's own state the
// agent cannot reach.
func StartupFilesPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".abhed", "startup-files.json"), nil
}

// TrustedStartupFiles is the baseline as recorded; none when there is no file.
func TrustedStartupFiles() ([]TrustedStartupFile, error) {
	path, err := StartupFilesPath()
	if err != nil {
		return nil, err
	}
	if n, err := nlink.Linked(path); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, nlink.Refusal(path, n)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the user's own state under their home
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f startupFiles
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return f.Files, nil
}

// TrustStartupFiles replaces the baseline with files.
func TrustStartupFiles(files []TrustedStartupFile) error {
	path, err := StartupFilesPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	data, err := json.MarshalIndent(startupFiles{Version: 1, Files: files}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".startup-files-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// StartupBaseline is the baseline as the policy reads it; none when it cannot
// be read, so every start-up file is judged.
func StartupBaseline() map[string]policy.StartupFile {
	files, err := TrustedStartupFiles()
	if err != nil || len(files) == 0 {
		return nil
	}
	out := make(map[string]policy.StartupFile, len(files))
	for _, f := range files {
		out[f.Path] = policy.StartupFile{Size: f.Size, SHA256: f.SHA256}
	}
	return out
}
