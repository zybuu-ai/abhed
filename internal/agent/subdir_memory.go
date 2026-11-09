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

	mu     sync.Mutex
	seen   map[string]bool
	loaded int
	capped bool
}

// maxSubdirMemory bounds how many folders' files one session loads, and
// maxSubdirSeen how many folders it remembers having looked in.
const (
	maxSubdirMemory = 32
	maxSubdirSeen   = 4096
)

// FolderMemoryTag is how a folder's memory is fenced in a tool result: the
// tag ends in a hash of the block, and the same tag in any tool's own output
// is escaped, so only Abhed can open one.
const FolderMemoryTag = "folder-memory-"

// discover returns the memory files of the folders paths lie in that were not
// looked in before, nearest last, so the most specific is read last; capped
// is set the first time the bound stops one loading.
func (m *SubdirMemory) discover(paths []string) (out []MemoryEntry, capped bool) {
	if m == nil || m.Workspace == "" {
		return nil, false
	}
	root := filepath.Clean(m.Workspace)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[string]bool{}
	}
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
			if m.seen[d] || len(m.seen) >= maxSubdirSeen || inStateFolder(root, d) {
				continue
			}
			f := filepath.Join(d, MemoryFileName)
			if !existsAsNamed(f) {
				m.seen[d] = true
				continue
			}
			if m.loaded >= maxSubdirMemory {
				if !m.capped {
					m.capped, capped = true, true
				}
				continue // left unseen: a later session may load it
			}
			m.seen[d] = true
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
			m.loaded++
			sum := sha256.Sum256(data)
			e.SHA256, e.Content = hex.EncodeToString(sum[:]), strings.TrimSpace(string(data))
			out = append(out, e)
		}
	}
	return out, capped
}

// note marks folders whose memory the conversation already holds.
func (m *SubdirMemory) note(labels []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[string]bool{}
	}
	for _, l := range labels {
		m.seen[filepath.Dir(filepath.Join(filepath.Clean(m.Workspace), filepath.FromSlash(l)))] = true
		m.loaded++
	}
}

// forget is compaction: what was loaded may be gone from the conversation,
// so each folder's memory loads again the next time it is used.
func (m *SubdirMemory) forget() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.seen, m.loaded, m.capped = nil, 0, false
	m.mu.Unlock()
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

// escapeFolderFence escapes every folder memory tag in tool output,
// wherever on its line, so none can pass for the fence.
func escapeFolderFence(s string) string {
	if !strings.Contains(s, FolderMemoryTag) {
		return s
	}
	s = strings.ReplaceAll(s, "</"+FolderMemoryTag, "\\</"+FolderMemoryTag)
	return strings.ReplaceAll(s, "<"+FolderMemoryTag, "\\<"+FolderMemoryTag)
}

// folderMemory is what a tool result gains for the folders call used: each
// folder's ABHED.md fenced and redacted, and a note of any not loaded. The
// loads are recorded as memory.loaded.
func (l *Loop) folderMemory(call model.ToolCall) string {
	var paths []string
	for _, p := range callPaths(call) {
		if !filepath.IsAbs(p) && l.Session != nil {
			p = filepath.Join(l.Session.WorkDir(), p)
		}
		paths = append(paths, p)
	}
	entries, capped := l.SubdirMemory.discover(paths)
	var b strings.Builder
	var files []MemoryFile
	for _, e := range entries {
		if e.Skipped != "" {
			fmt.Fprintf(&b, "\n\n[%s was not loaded as memory: %s]", e.Label, e.Skipped)
			files = append(files, MemoryFile{Path: e.Label, Scope: MemorySubdir, Skipped: e.Skipped})
			continue
		}
		body := escapeFolderFence(e.Content)
		sum := sha256.Sum256([]byte(body))
		tag := FolderMemoryTag + hex.EncodeToString(sum[:6])
		fmt.Fprintf(&b, "\n\n<%s path=%q>\n%s\n</%s>", tag, e.Label, body, tag)
		files = append(files, MemoryFile{Path: e.Label, Scope: MemorySubdir, SHA256: e.SHA256})
	}
	var note string
	if capped {
		note = fmt.Sprintf("this session has loaded %d folders' memory, the most it loads; later folders' ABHED.md are not loaded", maxSubdirMemory)
		fmt.Fprintf(&b, "\n\n[%s]", note)
	}
	if len(files) > 0 || note != "" {
		l.record(EvMemoryLoaded, ActorSystem, MemoryLoaded{Files: files, Note: note})
	}
	if b.Len() == 0 {
		return ""
	}
	// A secret in the file is stripped as in any tool result; a redactor
	// that fails withholds the block.
	if red := l.Recorder.redactor(); red != nil {
		return redactedText(red.Redact, b.String())
	}
	return b.String()
}

// NoteFolderMemory marks the folders whose memory a resumed or branched
// conversation holds since its last compaction, so it is not loaded twice.
func (l *Loop) NoteFolderMemory(events []Event) {
	if l.SubdirMemory == nil {
		return
	}
	var labels []string
	for _, e := range Live(events) {
		switch e.Type {
		case EvCompactDone:
			var c map[string]any
			if json.Unmarshal(e.Payload, &c) == nil && c["skipped"] == nil && c["error"] == nil {
				labels = nil
			}
		case EvMemoryLoaded:
			var m MemoryLoaded
			if json.Unmarshal(e.Payload, &m) != nil {
				continue
			}
			for _, f := range m.Files {
				if f.Scope == MemorySubdir && f.Skipped == "" {
					labels = append(labels, f.Path)
				}
			}
		}
	}
	l.SubdirMemory.note(labels)
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
