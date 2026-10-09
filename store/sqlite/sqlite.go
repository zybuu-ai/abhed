// Package sqlite is the control plane's store on one host: sandboxes,
// idempotency records and the append-only audit record in one SQLite file.
// It uses a pure-Go SQLite, so the binary needs no cgo.
//
// It is an experimental API until 2.0, and may change in a minor release.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	// The pure-Go SQLite driver, registered as "sqlite".
	_ "modernc.org/sqlite"

	"github.com/zybuu-ai/abhed/internal/nlink"
)

// DefaultName is the file's name in the default folder, ~/.abhed/controlplane.
const DefaultName = "sandboxes.db"

// busyTimeout is how long a statement waits for another process's write lock.
const busyTimeout = 5 * time.Second

// readers bounds the read connections; writes go through one connection.
const readers = 4

// Store is the control plane's store in SQLite for the one tenant a file
// holds. It passes controlplane/storetest.
type Store struct {
	path string
	// w is the only connection that writes, and every write is one
	// BEGIN IMMEDIATE transaction on it; r serves reads.
	w, r *sql.DB

	mu    sync.Mutex
	swept time.Time
}

// Files are the database file and the files SQLite keeps beside it.
func Files(path string) []string {
	return []string{path, path + "-wal", path + "-shm", path + "-journal"}
}

// Open opens the database at path, creating it, and its folder, when
// missing, and applies any migration this build has that the file lacks.
// The folder must be the server user's and closed to everyone else (0700),
// and the file the user's own (0600): a link, a hard link, or a file or
// folder others may write is refused, never repaired.
func Open(ctx context.Context, path string) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("the control plane's database %q is not an absolute path", path)
	}
	// The driver reads everything after a '?' as options.
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("the control plane's database %q has a '?' or '#' in its name", path)
	}
	path = filepath.Clean(path)
	dir, err := privateDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(dir, filepath.Base(path))
	before, err := prepareFile(path)
	if err != nil {
		return nil, err
	}
	for _, side := range Files(path)[1:] {
		if _, err := checkFile(side); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	s := &Store{path: path}
	common := "_pragma=busy_timeout(" + fmt.Sprint(busyTimeout.Milliseconds()) + ")&_pragma=foreign_keys(1)" +
		"&_pragma=trusted_schema(0)&_defensive=1&_dqs=0"
	s.w, err = sql.Open("sqlite", path+"?"+common+"&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open the control plane's database: %w", err)
	}
	s.w.SetMaxOpenConns(1)
	s.w.SetMaxIdleConns(1)
	s.w.SetConnMaxIdleTime(0)
	s.w.SetConnMaxLifetime(0)
	if err := s.migrate(ctx); err != nil {
		_ = s.w.Close()
		return nil, err
	}
	var mode string
	if err := s.w.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		_ = s.w.Close()
		return nil, fmt.Errorf("the control plane's database %s: reading its journal mode: %w", path, err)
	}
	if !strings.EqualFold(mode, "wal") {
		_ = s.w.Close()
		return nil, fmt.Errorf("the control plane's database %s is not in WAL mode (%q)", path, mode)
	}
	s.r, err = sql.Open("sqlite", path+"?"+common+"&_query_only=1")
	if err != nil {
		_ = s.w.Close()
		return nil, fmt.Errorf("open the control plane's database: %w", err)
	}
	s.r.SetMaxOpenConns(readers)
	// The name may not have been swapped for another file while it opened.
	after, err := checkFile(path)
	if err == nil && !os.SameFile(before, after) {
		err = fmt.Errorf("the control plane's database %s was replaced while it opened", path)
	}
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	for _, side := range Files(path)[1:] {
		if _, err := checkFile(side); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = s.Close()
			return nil, err
		}
	}
	return s, nil
}

// Path is the database file.
func (s *Store) Path() string { return s.path }

// Close closes both connection pools.
func (s *Store) Close() error {
	var errs []error
	if s.r != nil {
		errs = append(errs, s.r.Close())
	}
	if s.w != nil {
		errs = append(errs, s.w.Close())
	}
	return errors.Join(errs...)
}

// privateDir makes dir 0700 when it is missing and refuses one another user
// owns or others may enter. It returns dir with links resolved.
func privateDir(dir string) (string, error) {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return checkDir(dir)
}

// checkDir refuses an existing folder another user owns or others may
// enter, and returns it with links resolved.
func checkDir(dir string) (string, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	if err := ownedByMe(info); err != nil {
		return "", fmt.Errorf("the control plane's directory %s: %w", dir, err)
	}
	if !windows && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("the control plane's directory %s is open to other users (mode %04o); "+
			"run chmod 700 on it, or set control_plane.sqlite_path to a folder of its own", dir, info.Mode().Perm())
	}
	return real, nil
}

// checkFile refuses a database file that is a link, not a plain file,
// another user's, open to others, or reachable by a second name.
func checkFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return nil, fmt.Errorf("the control plane's database %s is a link; it must be the file itself", path)
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("the control plane's database %s is not a regular file", path)
	case !windows && info.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("the control plane's database %s is open to other users (mode %04o); run chmod 600 on it",
			path, info.Mode().Perm())
	}
	if err := ownedByMe(info); err != nil {
		return nil, fmt.Errorf("the control plane's database %s: %w", path, err)
	}
	if n := nlink.Of(info); n > 1 {
		return nil, fmt.Errorf("the control plane's database %s has %d names; it must have only its own", path, n)
	}
	return info, nil
}

// prepareFile checks the file, creating it 0600 when missing so SQLite never
// creates it under a looser umask.
func prepareFile(path string) (os.FileInfo, error) {
	info, err := checkFile(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return info, err
	}
	// O_EXCL also refuses a link planted at the name.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the configured state path
	if err != nil {
		return nil, fmt.Errorf("create the control plane's database: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return checkFile(path)
}
