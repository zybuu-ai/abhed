package identity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/filelock"
	"github.com/zybuu-ai/abhed/internal/home"
)

// Guest keeps nothing. Its folder is ~/.abhed/guest/<random>: inside the
// state the agent cannot reach, so one guest cannot read another's or an
// account's, and on the same volume as everything else. Every Abhed that
// enters guest holds a lock on <folder>/.lock for as long as it runs; the
// kernel drops the lock when the process ends however it ends, and the next
// start of any Abhed removes every guest folder whose lock is free.
//
// Removal is ordinary unlinking. On an SSD or APFS it is not a secure erase.

const guestLock = ".lock"

// newPrefix marks a guest folder being made: it is locked before it gets
// its final name, and a sweep leaves one alone while it is young.
const newPrefix = ".new-"

// youngNew is how long a folder being made is left alone by a sweep.
const youngNew = time.Minute

// GuestSession is a guest's folder while the guest works.
type GuestSession struct {
	Root   string
	unlock func()
	once   sync.Once
	err    error
}

func guestDir() (string, error) {
	dir, err := home.MachinePath(home.GuestDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "", fmt.Errorf("%s must be a folder, not a link", dir)
	}
	return dir, nil
}

// EnterGuest makes a guest folder, locks it and makes it the person's
// folder. End removes it.
func EnterGuest() (*GuestSession, error) {
	dir, err := guestDir()
	if err != nil {
		return nil, err
	}
	name := newID()
	tmp := filepath.Join(dir, newPrefix+name)
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return nil, err
	}
	unlock, err := filelock.Lock(filepath.Join(tmp, guestLock), 0, "%s is held (%s)")
	if err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	root := filepath.Join(dir, name)
	if err := os.Rename(tmp, root); err != nil {
		unlock()
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	if err := home.SetPerson(root); err != nil {
		unlock()
		_ = os.RemoveAll(root)
		return nil, err
	}
	return &GuestSession{Root: root, unlock: unlock}, nil
}

// End removes the guest's folder and everything in it, once.
func (g *GuestSession) End() error {
	if g == nil {
		return nil
	}
	g.once.Do(func() {
		if p, err := home.Person(); err == nil && p == g.Root {
			_ = home.SetPerson("")
		}
		g.err = os.RemoveAll(g.Root)
		g.unlock()
	})
	return g.err
}

// SweepGuests removes every guest folder no running Abhed holds: what a
// crash, a power loss or kill -9 left. It returns the folders it removed.
func SweepGuests() ([]string, error) {
	dir, err := home.MachinePath(home.GuestDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			// Nothing else belongs here; a link is removed, never followed.
			if err := os.Remove(p); err == nil {
				removed = append(removed, p)
			}
			continue
		}
		if strings.HasPrefix(e.Name(), newPrefix) && time.Since(fi.ModTime()) < youngNew {
			continue
		}
		unlock, err := filelock.Lock(filepath.Join(p, guestLock), 0, "%s is held (%s)")
		if err != nil {
			continue // a running guest holds it
		}
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err)
		} else {
			removed = append(removed, p)
		}
		unlock()
	}
	return removed, errors.Join(errs...)
}
