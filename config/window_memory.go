package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/nlink"
)

// LearnedWindowTTL is how long a limit a provider named is kept. An account
// is moved to a larger window from time to time; a limit older than this is
// learned again, at the cost of one refused call.
const LearnedWindowTTL = 30 * 24 * time.Hour

// learnedWindow is one limit a provider named for an endpoint's model.
type learnedWindow struct {
	Window int       `json:"window"`
	At     time.Time `json:"at"`
}

type learnedWindows struct {
	Version int                      `json:"version"`
	Windows map[string]learnedWindow `json:"windows"`
}

// ContextWindowsPath is ~/.abhed/context-windows.json: the limits providers
// named, kept in the person's own state, which the agent cannot reach and a
// workspace cannot set.
func ContextWindowsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".abhed", "context-windows.json"), nil
}

// WindowMemory keeps learned context windows in ContextWindowsPath. It
// satisfies model.WindowMemory. A file it cannot read or write is no memory:
// the limit is learned again, never a failed session.
type WindowMemory struct {
	// Path overrides ContextWindowsPath, for tests.
	Path string
	// Now overrides time.Now, for tests.
	Now func() time.Time

	mu sync.Mutex
}

func (m *WindowMemory) path() (string, error) {
	if m.Path != "" {
		return m.Path, nil
	}
	return ContextWindowsPath()
}

func (m *WindowMemory) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *WindowMemory) read(path string) learnedWindows {
	f := learnedWindows{Version: 1, Windows: map[string]learnedWindow{}}
	if n, err := nlink.Linked(path); err != nil || n > 0 {
		return f // a linked file is not the person's own; ignored
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the user's own state under their home
	if err != nil {
		return f
	}
	if json.Unmarshal(data, &f) != nil || f.Windows == nil {
		f.Windows = map[string]learnedWindow{}
	}
	return f
}

// Recall is the window remembered for key and when it was named, or 0 when
// none is, or it is older than LearnedWindowTTL.
func (m *WindowMemory) Recall(key string) (int, time.Time) {
	path, err := m.path()
	if err != nil {
		return 0, time.Time{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.read(path).Windows[key]
	if !ok || m.now().Sub(w.At) > LearnedWindowTTL {
		return 0, time.Time{}
	}
	return w.Window, w.At
}

// Remember keeps window for key, dropping entries past their time.
func (m *WindowMemory) Remember(key string, window int) {
	_ = m.change(key, window)
}

// Forget drops what is kept for key.
func (m *WindowMemory) Forget(key string) {
	_ = m.change(key, 0)
}

// change keeps window for key, or drops key when window is 0. Keys are
// hashes (model.WindowKey): no endpoint or credential is written.
func (m *WindowMemory) change(key string, window int) error {
	path, err := m.path()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	if n, err := nlink.Linked(path); err != nil {
		return err
	} else if n > 0 {
		return errors.New("context-windows.json is a link; not written")
	}
	f := m.read(path)
	now := m.now()
	for k, w := range f.Windows {
		if now.Sub(w.At) > LearnedWindowTTL {
			delete(f.Windows, k)
		}
	}
	f.Version = 1
	if window > 0 {
		f.Windows[key] = learnedWindow{Window: window, At: now}
	} else {
		delete(f.Windows, key)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".context-windows-*.json")
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
