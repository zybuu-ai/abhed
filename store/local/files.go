package local

import (
	"fmt"
	"io"
	"os"

	"github.com/zybuu-ai/abhed/internal/nlink"
)

// openOwn opens one of the record's own files: never through a link, never
// one with a second name, and made owner-only if it was found wider.
func openOwn(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, flag|oNoFollow, 0o600) // #nosec G304 -- a path the store builds from checked names
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a plain file", path)
	}
	if nlink.Of(info) > 1 {
		_ = f.Close()
		return nil, fmt.Errorf("%s has another name (a hard link); the record's files have one", path)
	}
	if info.Mode().Perm() != 0o600 {
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("make %s private: %w", path, err)
		}
	}
	return f, nil
}

// readOwn reads one of the record's own files as openOwn opens it.
func readOwn(path string) ([]byte, error) {
	f, err := openOwn(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}
