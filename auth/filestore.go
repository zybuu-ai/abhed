package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// FileUserStore keeps accounts in a JSON file next to the workspace config.
//
// This exists because the alternative was worse in a specific, discoverable
// way: with an in-memory store, `abhed user add` created an account inside a
// CLI process that then exited, and the server started with none. The command
// reported success and the user could not sign in. A pilot deployment with no
// Postgres is a legitimate configuration, and it needs somewhere real to put
// accounts.
//
// Postgres remains the right answer for anything multi-node — a file cannot be
// shared across replicas — and `abhed doctor` says so.
type FileUserStore struct {
	path string
	mu   sync.RWMutex
	gen  atomic.Uint64
}

func NewFileUserStore(path string) (*FileUserStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create account directory: %w", err)
	}
	return &FileUserStore{path: path}, nil
}

// Path reports where accounts are stored, so the CLI can tell an operator.
func (f *FileUserStore) Path() string { return f.path }

// storedUser is the on-disk shape.
//
// It exists solely because User.Hash is `json:"-"` — that tag is what
// guarantees a password hash can never fall out of an HTTP response, and it is
// worth keeping absolute. Persisting the hash therefore needs its own type
// rather than a relaxed tag on the shared one: the API guarantee holds by
// construction, and only this file can write a hash to disk.
type storedUser struct {
	User
	Hash string `json:"hash"`
}

func (f *FileUserStore) load() (map[string]*User, error) {
	data, err := os.ReadFile(f.path)
	if os.IsNotExist(err) {
		return map[string]*User{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.path, err)
	}
	var stored []storedUser
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("parse %s: %w", f.path, err)
	}
	out := make(map[string]*User, len(stored))
	for _, s := range stored {
		u := s.User
		u.Hash = s.Hash
		out[strings.ToLower(u.Username)] = &u
	}
	return out, nil
}

// save writes atomically. A half-written account file would lock every user
// out of the deployment, so the rename has to be the only visible step.
func (f *FileUserStore) save(users map[string]*User) error {
	list := make([]storedUser, 0, len(users))
	for _, u := range users {
		list = append(list, storedUser{User: *u, Hash: u.Hash})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Username < list[j].Username })

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	f.gen.Add(1)
	tmp := f.path + ".tmp"
	// 0600: the file holds password hashes. Group- or world-readable is a
	// standing offer to run bcrypt offline.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	return os.Rename(tmp, f.path)
}

func (f *FileUserStore) Get(_ context.Context, username string) (*User, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	users, err := f.load()
	if err != nil {
		return nil, err
	}
	u, found := users[strings.ToLower(username)]
	if !found {
		return nil, ErrNoSuchUser
	}
	return u, nil
}

func (f *FileUserStore) Put(_ context.Context, u *User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	users, err := f.load()
	if err != nil {
		return err
	}
	copy := *u
	users[strings.ToLower(u.Username)] = &copy
	return f.save(users)
}

func (f *FileUserStore) List(_ context.Context) ([]*User, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	users, err := f.load()
	if err != nil {
		return nil, err
	}
	out := make([]*User, 0, len(users))
	for _, u := range users {
		out = append(out, u)
	}
	return out, nil
}

func (f *FileUserStore) Delete(_ context.Context, username string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	users, err := f.load()
	if err != nil {
		return err
	}
	key := strings.ToLower(username)
	if _, found := users[key]; !found {
		return ErrNoSuchUser
	}
	delete(users, key)
	return f.save(users)
}

// Version changes whenever the file does, whichever process wrote it: its
// modification time and size, and a count of this process's own writes.
func (f *FileUserStore) Version() (string, error) {
	fi, err := os.Stat(f.path)
	if os.IsNotExist(err) {
		return fmt.Sprintf("%d:none", f.gen.Load()), nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d", f.gen.Load(), fi.ModTime().UnixNano(), fi.Size()), nil
}
