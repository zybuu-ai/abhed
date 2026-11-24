package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/home"
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

// ContextWindowsPath is context-windows.json in the folder of the person
// working (home.PersonPath): an account's own folder, or ~/.abhed before
// sign-in. It is asked on every use, so a sign-in during the process moves
// it. The agent cannot reach it and a workspace cannot set it.
func ContextWindowsPath() (string, error) {
	return home.PersonPath("context-windows.json")
}

// WindowMemory keeps learned context windows in ContextWindowsPath. It
// satisfies model.WindowMemory. A file it cannot read or write is no memory:
// the limit is learned again, never a failed session.
type WindowMemory struct {
	// Path overrides ContextWindowsPath, for tests.
	Path string
	// Now overrides time.Now, for tests.
	Now func() time.Time

	mu     sync.Mutex
	secret []byte
	// secretFrom is the file secret was read from: a secret is used only
	// for the folder it came from, never one read before a sign-in.
	secretFrom string
}

// secretPath is the key file beside the windows file:
// ~/.abhed/context-windows.key.
func (m *WindowMemory) secretPath() (string, error) {
	path, err := m.path()
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(path, filepath.Ext(path)) + ".key", nil
}

// Secret is the random 32-byte secret the memory's keys are made under
// (model.WindowKey), made once and kept in its own file, mode 0600. Without
// it the windows file confirms nothing about the credentials in its keys.
// nil when it cannot be read or made, or the file is a link or the wrong
// size: then the memory is not used, and limits are learned per process.
func (m *WindowMemory) Secret() []byte {
	path, err := m.secretPath()
	if err != nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.secret != nil && m.secretFrom == path {
		return m.secret
	}
	s, err := loadOrMakeSecret(path)
	if err != nil {
		return nil
	}
	m.secret, m.secretFrom = s, path
	return s
}

const secretSize = 32

func readSecret(path string) ([]byte, error) {
	if n, err := nlink.Linked(path); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, nlink.Refusal(path, n)
	}
	s, err := os.ReadFile(path) // #nosec G304 -- the user's own state under their home
	if err != nil {
		return nil, err
	}
	if len(s) != secretSize {
		return nil, fmt.Errorf("%s is not a %d-byte secret", path, secretSize)
	}
	return s, nil
}

// loadOrMakeSecret reads the secret, or makes it: written to a temporary
// file and linked into place, so no reader sees a partial one and two
// processes making it at once end with the same.
func loadOrMakeSecret(path string) ([]byte, error) {
	if s, err := readSecret(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return s, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	if s, err := readSecret(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return s, err
	}
	s := make([]byte, secretSize)
	if _, err := rand.Read(s); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".context-windows-key-*")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(s); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return nil, err
	}
	if err := os.Link(name, path); err != nil {
		return nil, err
	}
	_ = os.Remove(name)
	return readSecret(path)
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
	if _, ok := f.Windows[key]; window == 0 && !ok {
		return nil // nothing kept to forget: the file is left as it is
	}
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
