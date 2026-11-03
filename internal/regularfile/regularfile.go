// Package regularfile opens and reads files that must be plain files: a
// workspace's own state, which a repository or a command may have replaced
// with a link, a device or a pipe.
package regularfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// ErrNotRegular is a path that is not a plain file, or that changed to
// something else between being looked at and being opened.
var ErrNotRegular = errors.New("not a plain file")

// ErrTooLarge is a file larger than the bound it is read with.
var ErrTooLarge = errors.New("file too large")

// afterLstat runs between the look and the open; tests swap the file there.
var afterLstat = func(string) {}

// Open opens path for reading only when it is a plain file. A link at its
// last element is not followed, and a pipe or device put in its place is
// never waited on: the open does not block, and the file opened must be the
// one that was looked at.
func Open(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	afterLstat(path)
	f, err := openNoFollow(path)
	if errors.Is(err, syscall.ELOOP) {
		return nil, nil, fmt.Errorf("%s: %w", path, ErrNotRegular) // a link swapped in after the look
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err) // not there any more, or not permitted: kept as it is
	}
	now, err := f.Stat()
	if err != nil || !now.Mode().IsRegular() || !os.SameFile(before, now) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	return f, now, nil
}

// Read reads the plain file at path, refusing one larger than limit bytes.
func Read(path string, limit int64) ([]byte, error) {
	f, fi, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if fi.Size() > limit {
		return nil, fmt.Errorf("%s is %d bytes: %w (at most %d)", path, fi.Size(), ErrTooLarge, limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s grew past %d bytes: %w", path, limit, ErrTooLarge)
	}
	return data, nil
}
