package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/filelock"
)

// Store is the person's installed plugins: the files under Root, one folder
// per plugin and digest, and what was decided about them in StateFile.
type Store struct {
	Root      string
	StateFile string
}

// DefaultStore is ~/.abhed/plugins and ~/.abhed/plugins.json.
func DefaultStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Store{Root: filepath.Join(home, ".abhed", "plugins"), StateFile: filepath.Join(home, ".abhed", "plugins.json")}, nil
}

// State is the decisions file.
type State struct {
	// Publishers are the signers the person trusts when the managed
	// configuration names none.
	Publishers []Publisher `json:"publishers,omitempty"`
	// TrustedRoot is the person's Sigstore trusted_root.json, when the
	// managed configuration names none.
	TrustedRoot string            `json:"trusted_root,omitempty"`
	Plugins     map[string]*Entry `json:"plugins,omitempty"`
}

// Entry is one plugin: the content that loads, and content waiting for approval.
type Entry struct {
	Approved *Install `json:"approved,omitempty"`
	Pending  *Install `json:"pending,omitempty"`
}

// Install is one version of a plugin as installed.
type Install struct {
	Version   string `json:"version"`
	Publisher string `json:"publisher"`
	Digest    string `json:"digest"`
	Dir       string `json:"dir"`
	Basis     string `json:"basis"`
	// Signer is who signed, as verified at install, for the person to read.
	// Nothing trusts it: a load verifies the staged signature again.
	Signer *Signer `json:"signer,omitempty"`
	Pin    string  `json:"pin,omitempty"`
	// SourcePin is the digest a trusted source's index vouched for at
	// install: the person's approval came through that source.
	SourcePin string    `json:"source_pin,omitempty"`
	Source    string    `json:"source,omitempty"`
	From      string    `json:"from,omitempty"`
	Installed time.Time `json:"installed_at"`
	Approved  time.Time `json:"approved_at,omitzero"`
}

// Lock takes the store's lock for a change.
func (s *Store) Lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.StateFile), 0o700); err != nil {
		return nil, err
	}
	return filelock.Lock(s.StateFile+".lock", 10*time.Second, "another abhed plugin command is changing plugins")
}

// Read reads the state; no file is an empty state.
func (s *Store) Read() (*State, error) {
	st := &State{Plugins: map[string]*Entry{}}
	info, err := os.Lstat(s.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", s.StateFile)
	}
	data, err := os.ReadFile(s.StateFile)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("%s: %w", s.StateFile, err)
	}
	if st.Plugins == nil {
		st.Plugins = map[string]*Entry{}
	}
	// The folders named here are removed on approve and remove, so each must
	// be the one its digest names under its plugin, and nothing else.
	for name, e := range st.Plugins {
		if !nameRE.MatchString(name) || e == nil {
			return nil, fmt.Errorf("%s: %q is not a plugin name", s.StateFile, name)
		}
		for _, i := range []*Install{e.Approved, e.Pending} {
			if i != nil && (!hexRE.MatchString(i.Digest) || i.Dir != DirFor(i.Digest)) {
				return nil, fmt.Errorf("%s: plugin %s names a folder that is not its digest's", s.StateFile, name)
			}
		}
	}
	return st, nil
}

// TrustedRootFile is the person's own Sigstore trusted root.
func (s *Store) TrustedRootFile() string {
	return filepath.Join(filepath.Dir(s.StateFile), "plugin-trusted-root.json")
}

// Write replaces the state file whole.
func (s *Store) Write(st *State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeNew(s.StateFile, append(data, '\n'))
}

// writeNew writes data to a fresh, randomly named file beside p and renames
// it over p, so a link planted at p is replaced, never followed.
func writeNew(p string, data []byte) error {
	tmp := p + ".tmp-" + randomHex(8)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a fresh name beside the state file
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// DirFor is the folder name a bundle of digest goes in under its plugin:
// the whole digest, so no two bundles can share one.
func DirFor(digest string) string { return digest }

// Path is the absolute folder of an install.
func (s *Store) Path(name string, i *Install) string {
	return filepath.Join(s.Root, name, i.Dir)
}

// Stage writes b's files into a new folder and records it as pending. The
// approved version, if any, is left as it is.
func (s *Store) Stage(st *State, b *Bundle, inst Install) (*Install, error) {
	name := b.Manifest.Name
	inst.Version, inst.Publisher, inst.Digest, inst.Dir = b.Manifest.Version, b.Manifest.Publisher, b.Digest, DirFor(b.Digest)
	if inst.Installed.IsZero() {
		inst.Installed = time.Now().UTC()
	}
	e := st.Plugins[name]
	if e == nil {
		e = &Entry{}
		st.Plugins[name] = e
	}
	if e.Approved != nil && e.Approved.Digest == b.Digest {
		return nil, fmt.Errorf("%s %s is already installed with this exact content", name, b.Manifest.Version)
	}
	dest := filepath.Join(s.Root, name, inst.Dir)
	if err := os.MkdirAll(filepath.Join(s.Root, name), 0o700); err != nil {
		return nil, err
	}
	if err := refuseLink(s.Root); err != nil {
		return nil, err
	}
	tmp := dest + ".tmp-" + randomHex(6)
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return nil, err
	}
	all := map[string][]byte{ManifestName: b.ManifestBytes}
	if b.Signature != nil {
		all[SignatureName] = b.Signature
	}
	for p, d := range b.Files {
		all[p] = d
	}
	for p, d := range all {
		full := filepath.Join(tmp, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			_ = os.RemoveAll(tmp)
			return nil, err
		}
		if err := os.WriteFile(full, d, 0o600); err != nil {
			_ = os.RemoveAll(tmp)
			return nil, err
		}
	}
	// A pending folder of the same digest is replaced; any other pending one goes.
	if e.Pending != nil {
		_ = os.RemoveAll(filepath.Join(s.Root, name, e.Pending.Dir))
	}
	_ = os.RemoveAll(dest)
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	e.Pending = &inst
	return e.Pending, nil
}

// Approve makes name's pending content the content that loads. digest, when
// set, must be the pending content's.
func (s *Store) Approve(st *State, name, digest string) (old, now *Install, err error) {
	e := st.Plugins[name]
	if e == nil || e.Pending == nil {
		return nil, nil, fmt.Errorf("%s has nothing waiting for approval", name)
	}
	if digest != "" && digest != e.Pending.Digest {
		return nil, nil, fmt.Errorf("the content waiting for approval is %s, not %s", e.Pending.Digest, digest)
	}
	// What is approved is what is on disk, checked again now.
	if _, err := s.Open(name, e.Pending); err != nil {
		return nil, nil, err
	}
	old, now = e.Approved, e.Pending
	now.Approved = time.Now().UTC()
	e.Approved, e.Pending = now, nil
	if old != nil && old.Dir != now.Dir {
		_ = os.RemoveAll(filepath.Join(s.Root, name, old.Dir))
	}
	return old, now, nil
}

// Remove deletes every folder of name and its entry.
func (s *Store) Remove(st *State, name string) (*Entry, error) {
	e := st.Plugins[name]
	if e == nil {
		return nil, fmt.Errorf("%s is not installed", name)
	}
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%q is not a plugin name", name)
	}
	if err := refuseLink(s.Root); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(filepath.Join(s.Root, name)); err != nil {
		return nil, err
	}
	delete(st.Plugins, name)
	return e, nil
}

// Open reads an install's folder as a bundle and checks it is the content
// recorded: every file's hash, nothing added, the same manifest.
func (s *Store) Open(name string, i *Install) (*Bundle, error) {
	if i == nil {
		return nil, fmt.Errorf("%s has no content", name)
	}
	b, err := Open(s.Path(name, i))
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", name, err)
	}
	if b.Digest != i.Digest || b.Manifest.Name != name {
		return nil, fmt.Errorf("plugin %s: its manifest changed on disk after it was installed (digest %s, recorded %s)", name, b.Digest, i.Digest)
	}
	return b, nil
}

// Names are the installed plugins, sorted.
func (st *State) Names() []string {
	out := make([]string, 0, len(st.Plugins))
	for n := range st.Plugins {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func refuseLink(p string) error {
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a link; the plugins folder must be its own", p)
	}
	return nil
}

// ReadTrustedRoot reads and parses a trusted root file.
func ReadTrustedRoot(p string) (*TrustedRoot, error) {
	data, err := os.ReadFile(p) // #nosec G304 -- the trusted root the person or the managed file names
	if err != nil {
		return nil, fmt.Errorf("trusted root: %w", err)
	}
	return ParseTrustedRoot(data)
}

// expand replaces RootVar with root in s.
func expand(s, root string) string { return strings.ReplaceAll(s, RootVar, root) }
