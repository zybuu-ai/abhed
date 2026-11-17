// Package home answers where Abhed keeps state: the machine's folder,
// ~/.abhed, and the folder of the person working, which holds what belongs
// to them alone (the record, history, memory, secrets, sign-ins, trust).
//
// Until a process sets a person, the person's folder is the machine's, the
// layout every release before sign-in used. Both folders sit inside
// ~/.abhed, which the agent's tools and sandbox cannot reach, so the guard
// that protects one protects the other.
package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// DirName is the state folder's name, under the home folder and in a workspace.
const DirName = ".abhed"

// EnvPerson hands the person's folder to Abhed's own child processes.
const EnvPerson = "ABHED_PERSON_ROOT"

// Kinds of person folder, as the folder under ~/.abhed that holds them.
const (
	AccountsDir = "accounts"
	GuestDir    = "guest"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

var (
	mu     sync.RWMutex
	person string
)

// Machine is ~/.abhed.
func Machine() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if h == "" {
		return "", errors.New("no home folder")
	}
	return filepath.Join(h, DirName), nil
}

// MachinePath is a path under ~/.abhed.
func MachinePath(elem ...string) (string, error) {
	m, err := Machine()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{m}, elem...)...), nil
}

// Person is the folder of the person working: the one SetPerson chose, or
// the one a parent Abhed handed down in EnvPerson, or ~/.abhed. A handed-down
// folder that is not one Abhed makes is an error, never a fallback.
func Person() (string, error) {
	mu.RLock()
	p := person
	mu.RUnlock()
	if p != "" {
		return p, nil
	}
	if env := os.Getenv(EnvPerson); env != "" {
		if err := Check(env); err != nil {
			return "", fmt.Errorf("%s: %w", EnvPerson, err)
		}
		return filepath.Clean(env), nil
	}
	return Machine()
}

// PersonPath is a path under the person's folder.
func PersonPath(elem ...string) (string, error) {
	p, err := Person()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{p}, elem...)...), nil
}

// Legacy reports whether the person's folder is the machine's: no one has
// been chosen, as before sign-in existed.
func Legacy() bool {
	p, err := Person()
	m, merr := Machine()
	return err == nil && merr == nil && p == m
}

// Check reports why root cannot be a person's folder: it must be
// ~/.abhed/accounts/<name> or ~/.abhed/guest/<name>, a real folder and not a
// link, and private to its owner.
func Check(root string) error {
	m, err := Machine()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("%q is not a clean absolute path", root)
	}
	rel, err := filepath.Rel(m, root)
	if err != nil {
		return fmt.Errorf("%q is not under %s", root, m)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 || (parts[0] != AccountsDir && parts[0] != GuestDir) || !validName.MatchString(parts[1]) {
		return fmt.Errorf("%q is not a folder under %s or %s", root,
			filepath.Join(m, AccountsDir), filepath.Join(m, GuestDir))
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%q must be a folder, not a link", root)
	}
	if fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		return fmt.Errorf("%q is open to other users (mode %o)", root, fi.Mode().Perm())
	}
	return nil
}

// SetPerson makes root the person's folder for this process and the Abhed
// processes it starts. "" goes back to ~/.abhed.
func SetPerson(root string) error {
	if root != "" {
		if err := Check(root); err != nil {
			return err
		}
	}
	mu.Lock()
	defer mu.Unlock()
	person = root
	if root == "" {
		return os.Unsetenv(EnvPerson)
	}
	return os.Setenv(EnvPerson, root)
}

// ExpandState resolves a configured "~/.abhed/..." path to the person's
// folder, and any other "~/..." to the home folder.
func ExpandState(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"+DirName+"/"); ok {
		if q, err := PersonPath(filepath.FromSlash(rest)); err == nil {
			return q
		}
		return p
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			return filepath.Join(h, rest)
		}
	}
	return p
}
