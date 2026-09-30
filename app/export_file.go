package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zybuu-ai/abhed/internal/nlink"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// exportsDir is where exports go by default: ~/.abhed/exports.
func exportsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".abhed", "exports"), nil
}

// inExports reports whether path is in the exports folder, by its real path.
func inExports(path string) bool {
	dir, err := exportsDir()
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(tools.RealPath(dir), tools.RealPath(path))
	return err == nil && filepath.IsLocal(rel)
}

// openExport opens where an export is written. It never follows a link at
// the last step, never writes over a file that has another name, and never
// writes into Abhed's state, the record above all; the exports folder is
// the one part of ~/.abhed it writes to. A new file is created exclusively.
func openExport(path string, roots ...string) (*os.File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if !inExports(abs) && tools.IsState(abs, roots...) {
		return nil, fmt.Errorf("%s is Abhed's own state; an export is never written there", abs)
	}
	flags := os.O_WRONLY | os.O_CREATE | oNoFollow
	info, err := os.Lstat(abs)
	switch {
	case errors.Is(err, os.ErrNotExist):
		flags |= os.O_EXCL
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("%s is not a plain file (a link or a folder); an export does not write through it", abs)
	case nlink.Of(info) > 1:
		return nil, fmt.Errorf("%s has another name (a hard link); an export does not write over it", abs)
	default:
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(abs, flags, 0o600) // #nosec G304 -- checked above, and opened without following a link
	if err != nil {
		return nil, fmt.Errorf("write %s: %w", abs, err)
	}
	// What was opened is what was checked: a plain file with one name.
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || nlink.Of(st) > 1 || (info != nil && !os.SameFile(info, st)) {
		_ = f.Close()
		return nil, fmt.Errorf("%s changed while it was opened; not written", abs)
	}
	return f, nil
}
