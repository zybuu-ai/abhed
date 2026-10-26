package lsprelay

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// RustAnalyzerConfigNames are the workspace files rust-analyzer reads its
// settings from ahead of the editor's (and so the engine's).
var RustAnalyzerConfigNames = []string{"rust-analyzer.toml", ".rust-analyzer.toml"}

// Bounds on the walk for rust-analyzer.toml files. Past either, not every
// folder was checked, which is reported, never taken as "none".
var (
	raMaxEntries = 50000
	raMaxDepth   = 12
)

const raMaxFound = 16

// raSkipped are folders the walk leaves out: build output, packages and
// the repository's own store, which hold no crate rust-analyzer loads.
var raSkipped = []string{"target", "node_modules", ".git"}

// RustAnalyzerConfigs are the rust-analyzer.toml files that would override
// the engine's settings for a server on roots: in each root, in a bounded
// walk of its folders that follows no link and skips raSkipped, and in each
// root's ancestors, which rust-analyzer also reads. Any entry with the name
// that is not a folder counts, a link included (rust-analyzer reads through
// it). incomplete is set, saying why, when the walk stopped at a bound, so
// a file may have gone unseen.
func RustAnalyzerConfigs(roots []string) (files []string, incomplete string) {
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] && len(files) < raMaxFound {
			seen[p] = true
			files = append(files, p)
		}
	}
	for _, r := range roots {
		r = filepath.Clean(r)
		for _, dir := range append([]string{r}, ancestors(r)...) {
			for _, n := range RustAnalyzerConfigNames {
				if p := filepath.Join(dir, n); isConfigEntry(p) {
					add(p)
				}
			}
		}
		entries := 0
		_ = filepath.WalkDir(r, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // an unreadable folder is one the server cannot read either
			}
			if entries++; entries > raMaxEntries {
				incomplete = fmt.Sprintf("more than %d entries in %s", raMaxEntries, r)
				return filepath.SkipAll
			}
			if d.IsDir() {
				rel, _ := filepath.Rel(r, p)
				if rel == "." {
					return nil
				}
				if slices.Contains(raSkipped, d.Name()) {
					return filepath.SkipDir
				}
				if strings.Count(filepath.ToSlash(rel), "/") >= raMaxDepth-1 {
					incomplete = fmt.Sprintf("folders more than %d levels deep in %s", raMaxDepth, r)
					return filepath.SkipDir
				}
				return nil
			}
			if slices.Contains(RustAnalyzerConfigNames, d.Name()) {
				add(p)
			}
			return nil
		})
	}
	return files, incomplete
}

// ancestors are dir's parents, nearest first.
func ancestors(dir string) []string {
	var out []string
	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		out = append(out, d)
		if filepath.Dir(d) == d {
			return out
		}
	}
}

func isConfigEntry(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && !fi.IsDir()
}

// RustAnalyzerConfigWhy is what the person is told about such files, or
// about a check that could not look at every folder.
func RustAnalyzerConfigWhy(files []string, incomplete string) string {
	var parts []string
	if len(files) > 0 {
		parts = append(parts, "the workspace's "+strings.Join(files, ", ")+" overrides Abhed's rust-analyzer settings: it can turn build scripts, proc macros and "+
			"check on save back on, and name commands to run (cargo.buildScripts.overrideCommand, check.overrideCommand), inside the sandbox")
	}
	if incomplete != "" {
		parts = append(parts, "Abhed could not check every folder for a rust-analyzer.toml ("+incomplete+"), and one would override its rust-analyzer settings: "+
			"build scripts, proc macros and commands it names could run inside the sandbox")
	}
	return strings.Join(parts, "; ")
}

// RustAnalyzerConfigCache keeps one session's RustAnalyzerConfigs result,
// so a restart does not walk the tree again. It holds only while the stamp
// is the same (the session record's last event, which moves with every
// action), and Invalidate drops it: on a watch firing, or on anything that
// may have written to the workspace.
type RustAnalyzerConfigCache struct {
	mu         sync.Mutex
	key        string
	valid      bool
	files      []string
	incomplete string
}

// Get is RustAnalyzerConfigs(roots), from the cache when it holds them
// under the same stamp.
func (c *RustAnalyzerConfigCache) Get(roots []string, stamp string) ([]string, string) {
	key := stamp + "\x00" + strings.Join(roots, "\x00")
	c.mu.Lock()
	if c.valid && c.key == key {
		files, incomplete := c.files, c.incomplete
		c.mu.Unlock()
		return files, incomplete
	}
	c.mu.Unlock()
	files, incomplete := RustAnalyzerConfigs(roots)
	c.mu.Lock()
	c.key, c.valid, c.files, c.incomplete = key, true, files, incomplete
	c.mu.Unlock()
	return files, incomplete
}

// Invalidate makes the next Get walk again.
func (c *RustAnalyzerConfigCache) Invalidate() {
	c.mu.Lock()
	c.valid = false
	c.mu.Unlock()
}

// RustAnalyzerWatchEvery is how often a running rust-analyzer's watch looks.
var RustAnalyzerWatchEvery = 10 * time.Second

// RustAnalyzerWatch notes what a running rust-analyzer's settings could
// come from, cheaply: the config names in each root and its ancestors, and
// the files found when it started. Changed reports a new, changed or
// removed one since.
type RustAnalyzerWatch struct {
	paths []string
	seen  map[string]string
}

// NewRustAnalyzerWatch notes the paths for roots and the files found.
func NewRustAnalyzerWatch(roots, files []string) *RustAnalyzerWatch {
	w := &RustAnalyzerWatch{}
	add := map[string]bool{}
	for _, r := range roots {
		r = filepath.Clean(r)
		for _, dir := range append([]string{r}, ancestors(r)...) {
			for _, n := range RustAnalyzerConfigNames {
				add[filepath.Join(dir, n)] = true
			}
		}
	}
	for _, f := range files {
		add[f] = true
	}
	for p := range add {
		w.paths = append(w.paths, p)
	}
	slices.Sort(w.paths)
	w.seen = w.state()
	return w
}

func (w *RustAnalyzerWatch) state() map[string]string {
	out := map[string]string{}
	for _, p := range w.paths {
		if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
			out[p] = fmt.Sprintf("%d|%s|%d", fi.ModTime().UnixNano(), fi.Mode(), fi.Size())
		}
	}
	return out
}

// Changed reports whether any watched path is new, changed or gone.
func (w *RustAnalyzerWatch) Changed() bool {
	now := w.state()
	if len(now) != len(w.seen) {
		return true
	}
	for p, v := range now {
		if w.seen[p] != v {
			return true
		}
	}
	return false
}

// IsRustAnalyzerConfig reports whether p names a rust-analyzer.toml.
func IsRustAnalyzerConfig(p string) bool {
	return slices.Contains(RustAnalyzerConfigNames, path.Base(filepath.ToSlash(p)))
}

// raEditorMethods are the editor's messages that report a file being
// opened, saved, changed, created or renamed.
var raEditorMethods = map[string]bool{"textDocument/didOpen": true, "textDocument/didSave": true, "textDocument/didChange": true,
	"workspace/didChangeWatchedFiles": true, "workspace/didCreateFiles": true, "workspace/didRenameFiles": true}

// NamesRustAnalyzerConfig reports whether an editor message reports a
// rust-analyzer.toml opened, saved, changed, created or renamed: once the
// server has seen it, its settings may no longer be the engine's.
func NamesRustAnalyzerConfig(body []byte) bool {
	if !strings.Contains(string(body), "rust-analyzer.toml") {
		return false
	}
	var m struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(body, &m) != nil || !raEditorMethods[m.Method] {
		return false
	}
	v, err := decode(m.Params)
	if err != nil {
		return false
	}
	return namesConfig(v)
}

func namesConfig(v any) bool {
	switch x := v.(type) {
	case []any:
		return slices.ContainsFunc(x, namesConfig)
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && uriKeys[k] && IsRustAnalyzerConfig(s) {
				return true
			}
			if namesConfig(e) {
				return true
			}
		}
	}
	return false
}
