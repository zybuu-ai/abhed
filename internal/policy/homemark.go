package policy

import (
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// homeMark is a start-up file as it was when the session started.
type homeMark struct {
	size  int64
	mtime time.Time
	sum   [sha256.Size]byte
}

// homeMarks are the start-up files in the commands' HOME, marked once.
type homeMarks struct {
	once  sync.Once
	marks map[string]homeMark
}

// markHome records the HOME's start-up files at the first bash call, before
// it runs: the person's own, not judged while they stay unchanged.
func (e *Engine) markHome() {
	if e.homeMarked == nil {
		return
	}
	e.homeMarked.once.Do(func() {
		dir, visible := e.home()
		if !visible || !filepath.IsAbs(dir) {
			return
		}
		marks := map[string]homeMark{}
		for _, n := range tools.HomeStartupNames() {
			p := filepath.Join(dir, n)
			if m, ok := markOf(p); ok {
				marks[p] = m
			}
		}
		e.homeMarked.marks = marks
	})
}

// unchangedHomeFile reports a start-up file the same as when it was marked.
func (e *Engine) unchangedHomeFile(p string) bool {
	if e.homeMarked == nil {
		return false
	}
	was, ok := e.homeMarked.marks[p]
	if !ok {
		return false
	}
	now, ok := markOf(p)
	return ok && now == was
}

// markOf is a regular file's size, time and hash; false for anything else.
func markOf(p string) (homeMark, bool) {
	f, err := os.Open(p) // #nosec G304 -- a start-up file in the commands' HOME
	if err != nil {
		return homeMark{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > tools.MaxScriptBytes {
		return homeMark{}, false
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return homeMark{}, false
	}
	m := homeMark{size: info.Size(), mtime: info.ModTime()}
	copy(m.sum[:], h.Sum(nil))
	return m, true
}
