package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Probe is what Inspect found, without changing anything.
type Probe struct {
	Path string
	// Exists is false when Open would create the file.
	Exists bool
	// Version is the file's schema version; Open migrates it to SchemaVersion.
	Version int
}

// String says what Open would do with the file.
func (p Probe) String() string {
	switch {
	case !p.Exists:
		return "would be created at " + p.Path
	case p.Version < SchemaVersion:
		return fmt.Sprintf("%s, schema version %d; serve will migrate it to %d", p.Path, p.Version, SchemaVersion)
	}
	return fmt.Sprintf("%s, schema version %d", p.Path, p.Version)
}

// Inspect applies Open's checks to path without creating, migrating or
// writing anything: a missing folder or file is reported, an existing file
// is opened read-only for its schema version.
func Inspect(ctx context.Context, path string) (Probe, error) {
	if !filepath.IsAbs(path) {
		return Probe{}, fmt.Errorf("the control plane's database %q is not an absolute path", path)
	}
	if strings.ContainsAny(path, "?#") {
		return Probe{}, fmt.Errorf("the control plane's database %q has a '?' or '#' in its name", path)
	}
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		// Open creates it 0700; what lies above it is judged by the state-path rules.
		return Probe{Path: path}, nil
	}
	dir, err := checkDir(dir)
	if err != nil {
		return Probe{}, err
	}
	path = filepath.Join(dir, filepath.Base(path))
	if _, err := checkFile(path); errors.Is(err, os.ErrNotExist) {
		return Probe{Path: path}, nil
	} else if err != nil {
		return Probe{}, err
	}
	logged := false
	for _, side := range Files(path)[1:] {
		_, err := checkFile(side)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Probe{}, err
		}
		logged = logged || err == nil
	}
	db, err := sql.Open("sqlite", readOnlyURI(path, !logged))
	if err != nil {
		return Probe{}, fmt.Errorf("open the control plane's database: %w", err)
	}
	defer func() { _ = db.Close() }()
	p := Probe{Path: path, Exists: true}
	var tables int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_version'`).Scan(&tables); err != nil {
		return Probe{}, fmt.Errorf("read the control plane's database: %w", err)
	}
	if tables == 1 {
		if err := db.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_version`).Scan(&p.Version); err != nil {
			return Probe{}, fmt.Errorf("read the control plane's schema version: %w", err)
		}
	}
	if p.Version > SchemaVersion {
		return Probe{}, fmt.Errorf("the control plane's database %s is at schema version %d, newer than this build's %d; "+
			"run the Abhed that wrote it", path, p.Version, SchemaVersion)
	}
	return p, nil
}

// readOnlyURI names path so SQLite opens it read-only and never creates it.
// With no log beside it the file holds everything, and immutable keeps a WAL
// database from gaining -wal and -shm files just by being read.
func readOnlyURI(path string, whole bool) string {
	p := filepath.ToSlash(path)
	if runtime.GOOS == "windows" {
		p = "/" + p
	}
	q := "?mode=ro&_pragma=query_only(1)"
	if whole {
		q += "&immutable=1"
	}
	return "file:" + (&url.URL{Path: p}).EscapedPath() + q
}
