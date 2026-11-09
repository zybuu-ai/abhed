package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// SubdirMemory loads a folder's own ABHED.md the first time the agent works
// in it (reads, edits, writes or searches a path there): the folders from the
// one it touched up to, not including, the workspace root, each once. The
// workspace root's ABHED.md is in the system prompt already.
type SubdirMemory struct {
	// Workspace is the root the folders lie under.
	Workspace string
	// Allow, when set, is asked before a file is read, as the read rules
	// ask of the workspace's other memory files.
	Allow func(path string) error

	mu   sync.Mutex
	seen map[string]bool
}

// maxSubdirMemory bounds how many folders' files one session loads.
const maxSubdirMemory = 32

// discover returns the memory files of the folders paths lie in that were not
// loaded before, nearest last, so the most specific is read last.
func (m *SubdirMemory) discover(paths []string) []MemoryEntry {
	if m == nil || m.Workspace == "" {
		return nil
	}
	root := filepath.Clean(m.Workspace)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[string]bool{}
	}
	var out []MemoryEntry
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		p = filepath.Clean(p)
		rel, err := filepath.Rel(root, p)
		if err != nil || !filepath.IsLocal(rel) {
			continue // outside the workspace
		}
		dir := p
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			dir = filepath.Dir(p)
		}
		var chain []string
		for d := dir; d != root && strings.HasPrefix(d, root+string(filepath.Separator)); d = filepath.Dir(d) {
			chain = append(chain, d)
		}
		for i := len(chain) - 1; i >= 0; i-- {
			d := chain[i]
			if m.seen[d] || len(m.seen) >= maxSubdirMemory || inStateFolder(root, d) {
				continue
			}
			m.seen[d] = true
			f := filepath.Join(d, MemoryFileName)
			if !existsAsNamed(f) {
				continue
			}
			label, _ := filepath.Rel(root, f)
			e := MemoryEntry{Path: f, Scope: MemorySubdir, Label: filepath.ToSlash(label)}
			if m.Allow != nil {
				if err := m.Allow(f); err != nil {
					e.Skipped = err.Error()
					out = append(out, e)
					continue
				}
			}
			data, err := ReadMemoryFile(root, f)
			if err != nil {
				e.Skipped = err.Error()
				out = append(out, e)
				continue
			}
			sum := sha256.Sum256(data)
			e.SHA256, e.Content = hex.EncodeToString(sum[:]), strings.TrimSpace(string(data))
			out = append(out, e)
		}
	}
	return out
}

// callPaths are the paths a tool call names, as its arguments give them.
func callPaths(call model.ToolCall) []string {
	var args map[string]any
	if json.Unmarshal(call.Args, &args) != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"path", "file_path", "dir", "directory", "cwd"} {
		if v, ok := args[k].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

// attachSubdirMemory adds, to the result of the first call that touched a
// folder, that folder's ABHED.md, and records which files were loaded.
func (l *Loop) attachSubdirMemory(calls []model.ToolCall, results []callOutcome) {
	if l.SubdirMemory == nil {
		return
	}
	var files []MemoryFile
	for i, call := range calls {
		if i >= len(results) || !results[i].set || results[i].result.IsError {
			continue
		}
		var paths []string
		for _, p := range callPaths(call) {
			if !filepath.IsAbs(p) && l.Session != nil {
				p = filepath.Join(l.Session.WorkDir(), p)
			}
			paths = append(paths, p)
		}
		entries := l.SubdirMemory.discover(paths)
		var b strings.Builder
		for _, e := range entries {
			if e.Skipped != "" {
				continue
			}
			fmt.Fprintf(&b, "\n\n## Memory for %s (loaded because this folder was used)\n%s", filepath.ToSlash(filepath.Dir(e.Label)), e.Content)
			files = append(files, MemoryFile{Path: e.Label, Scope: MemorySubdir, SHA256: e.SHA256})
		}
		if b.Len() > 0 {
			results[i].result.Content += b.String()
		}
	}
	if len(files) > 0 {
		l.record(EvMemoryLoaded, ActorSystem, MemoryLoaded{Files: files})
	}
}

// inStateFolder reports a folder in Abhed's state or git's, whose files are
// never memory.
func inStateFolder(root, d string) bool {
	rel, err := filepath.Rel(root, d)
	if err != nil {
		return true
	}
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	return first == tools.StateDir || first == ".git"
}
