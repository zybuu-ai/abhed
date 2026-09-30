package ui

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// History is the prompts a person has submitted, newest last, kept per
// workspace on disk so Up and Ctrl-R reach yesterday's prompts too.
//
// The file lives under the user's home, not in the workspace: the agent can
// write the workspace, and a history file there could be seeded with a prompt
// the person would recall and send without reading.
type History struct {
	mu      sync.Mutex
	entries []string
	path    string // "" keeps history in memory only
}

// historyMax bounds what is loaded and kept.
const historyMax = 1000

// HistoryPath is where the history for workspace is kept: one file per
// workspace, named by a hash of its path.
func HistoryPath(workspace string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(filepath.Clean(workspace)))
	return filepath.Join(home, ".abhed", "history", hex.EncodeToString(sum[:8])+".jsonl")
}

// LoadHistory reads the history at path; a missing or unreadable file is an
// empty history, never an error that stops the prompt.
func LoadHistory(path string) *History {
	h := &History{path: path}
	if path == "" {
		return h
	}
	f, err := os.Open(path) // #nosec G304 -- the user's own history under their home
	if err != nil {
		return h
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		var e struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && strings.TrimSpace(e.Text) != "" {
			h.entries = append(h.entries, e.Text)
		}
	}
	if len(h.entries) > 2*historyMax {
		h.entries = h.entries[len(h.entries)-historyMax:]
		h.rewrite() // an append-only file would otherwise grow without end
	}
	if len(h.entries) > historyMax {
		h.entries = h.entries[len(h.entries)-historyMax:]
	}
	return h
}

// rewrite replaces the file with the entries kept, through a temporary file
// so a crash leaves the old one whole.
func (h *History) rewrite() {
	tmp := h.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- the user's own history under their home
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	for _, e := range h.entries {
		line, _ := json.Marshal(struct {
			Text string `json:"text"`
		}{e})
		_, _ = w.Write(append(line, '\n'))
	}
	if w.Flush() != nil || f.Close() != nil {
		_ = os.Remove(tmp)
		return
	}
	_ = os.Rename(tmp, h.path)
}

// Entries returns a copy of the history, oldest first.
func (h *History) Entries() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.entries...)
}

// Add records s, skipping a repeat of the last entry. persist writes it to
// disk too; a line cleared with Ctrl-C is remembered for this session only.
func (h *History) Add(s string, persist bool) {
	if strings.TrimSpace(s) == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(h.entries); n > 0 && h.entries[n-1] == s {
		return
	}
	h.entries = append(h.entries, s)
	if len(h.entries) > historyMax {
		h.entries = h.entries[len(h.entries)-historyMax:]
	}
	if !persist || h.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	line, _ := json.Marshal(struct {
		Text string `json:"text"`
	}{s})
	_, _ = f.Write(append(line, '\n'))
}
