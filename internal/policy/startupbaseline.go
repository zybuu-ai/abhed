package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// StartupFile is a start-up file the person trusted with abhed trust
// startup-files, by its size and sha256.
type StartupFile struct {
	Size   int64
	SHA256 string
}

// TrustedStartupFile is a start-up file left unjudged because it matches the
// baseline, as the session's record notes it.
type TrustedStartupFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// trustedStartup reports a start-up file that is, as a regular file and not
// a link, what the baseline holds for its path.
func (e *Engine) trustedStartup(p string) bool {
	want, ok := e.StartupBaseline[p]
	if !ok {
		return false
	}
	size, sum, ok := StartupFileMark(p)
	return ok && size == want.Size && sum == want.SHA256
}

// TrustedStartupFiles are the baseline's files in the commands' HOME that
// match it now.
func (e *Engine) TrustedStartupFiles() []TrustedStartupFile {
	dir, visible := e.home()
	if !visible || !filepath.IsAbs(dir) || len(e.StartupBaseline) == 0 {
		return nil
	}
	var out []TrustedStartupFile
	for _, n := range tools.HomeStartupNames() {
		p := filepath.Join(dir, n)
		if e.trustedStartup(p) {
			out = append(out, TrustedStartupFile{Path: p, SHA256: e.StartupBaseline[p].SHA256})
		}
	}
	return out
}

// StartupFileMark is a regular file's size and sha256; false for a link, a
// folder, anything else, or one too long to read as a script.
func StartupFileMark(p string) (int64, string, bool) {
	before, err := os.Lstat(p)
	if err != nil || !before.Mode().IsRegular() || before.Size() > tools.MaxScriptBytes {
		return 0, "", false
	}
	f, err := os.Open(p) // #nosec G304 -- a start-up file in the commands' HOME
	if err != nil {
		return 0, "", false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) {
		return 0, "", false
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, tools.MaxScriptBytes+1))
	if err != nil || n > tools.MaxScriptBytes {
		return 0, "", false
	}
	return n, hex.EncodeToString(h.Sum(nil)), true
}
