package tools

import (
	"fmt"
	"os"
)

// CanWriteFetched reports why a file fetched from elsewhere could not be
// written to path, as write would refuse it: Abhed's git state, the guard,
// or an existing file the session has not read.
func (s *Session) CanWriteFetched(path string) error {
	if err := gitWrite(path); err != nil {
		return err
	}
	if err := s.guard(path); err != nil {
		return fmt.Errorf("%s %w", NotApplied, err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.IsDir() {
			return fmt.Errorf("%s is a directory; give a file path", s.Rel(path))
		}
		if !s.WasRead(path) {
			return fmt.Errorf("refusing to replace %s: it exists but has not been read this session. "+
				"Read it first, or fetch to a new path", s.Rel(path))
		}
	}
	return nil
}

// WriteFetched writes data fetched from elsewhere to path, a path Resolve
// returned, as write writes: inside its root, with a checkpoint for /undo.
// A new file is readable by its owner only. It says whether it created or
// replaced the file.
func (s *Session) WriteFetched(path string, data []byte) (string, error) {
	if err := s.CanWriteFetched(path); err != nil {
		return "", err
	}
	mode, verb := os.FileMode(0o600), "Created"
	if info, err := os.Stat(path); err == nil {
		mode, verb = info.Mode().Perm(), "Replaced"
	} else if err := s.mkdirParents(path); err != nil {
		return "", fmt.Errorf("cannot create the folder for %s: %w", s.Rel(path), err)
	}
	s.recordChange(path)
	if err := s.atomicWrite(path, data, mode); err != nil {
		s.pruneMade(path)
		return "", fmt.Errorf("write failed for %s: %w", s.Rel(path), err)
	}
	return verb, nil
}
