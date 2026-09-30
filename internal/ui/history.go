package ui

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	// redact is applied to what is written to disk: a secret typed or
	// pasted into a prompt is withheld from the file as it is from the record.
	redact func(string) string
}

// historyMax bounds what is loaded and kept; historyEntryMax bounds one
// entry on disk, and a longer line in an older file is skipped, not fatal.
const (
	historyMax      = 1000
	historyEntryMax = 64 << 10
	historyLineMax  = 16 << 20
)

// HistoryPath is where the history for workspace is kept: one file per
// workspace, named by a hash of its resolved path, so a workspace reached
// through a link shares its history.
func HistoryPath(workspace string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	ws := filepath.Clean(workspace)
	if real, err := filepath.EvalSymlinks(ws); err == nil {
		ws = real
	}
	sum := sha256.Sum256([]byte(ws))
	return filepath.Join(home, ".abhed", "history", hex.EncodeToString(sum[:8])+".jsonl")
}

// LoadHistory reads the history at path; a missing or unreadable file is an
// empty history, never an error that stops the prompt.
func LoadHistory(path string) *History {
	h := &History{path: path}
	if path == "" {
		return h
	}
	f, err := os.OpenFile(path, os.O_RDONLY|noFollow, 0) // #nosec G304 -- the user's own history under their home
	if err != nil {
		return h
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	lines := 0
	for {
		line, err := readLine(br, historyLineMax)
		if line != nil {
			lines++
			var e struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(line, &e) == nil && strings.TrimSpace(e.Text) != "" {
				h.entries = append(h.entries, e.Text)
			}
		}
		if err != nil {
			break
		}
	}
	if len(h.entries) > historyMax {
		h.entries = h.entries[len(h.entries)-historyMax:]
	}
	if lines > 2*historyMax {
		h.rewrite() // an append-only file would otherwise grow without end
	}
	return h
}

// readLine reads one line; a line longer than max is skipped whole and
// returned as nil, so one oversized entry does not end the reading.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	over := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !over {
			line = append(line, chunk...)
			if len(line) > max {
				over, line = true, nil
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if over {
			return nil, err
		}
		if len(line) == 0 && err != nil {
			return nil, err
		}
		return line, err
	}
}

// SetRedact sets what is applied to an entry before it is written to disk.
func (h *History) SetRedact(f func(string) string) {
	h.mu.Lock()
	h.redact = f
	h.mu.Unlock()
}

// rewrite replaces the file with the entries kept, through a temporary file
// so a crash leaves the old one whole.
func (h *History) rewrite() {
	tmp := h.path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|noFollow, 0o600) // #nosec G304 -- the user's own history under their home
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	for _, e := range h.entries {
		_, _ = w.Write(append(h.encode(e), '\n'))
	}
	if w.Flush() != nil || f.Close() != nil {
		_ = os.Remove(tmp)
		return
	}
	_ = os.Rename(tmp, h.path)
}

// encode is an entry as it is stored: redacted, and cut to historyEntryMax.
func (h *History) encode(s string) []byte {
	if h.redact != nil {
		s = h.redact(s)
	}
	if len(s) > historyEntryMax {
		s = strings.ToValidUTF8(s[:historyEntryMax], "")
	}
	line, _ := json.Marshal(struct {
		Text string `json:"text"`
	}{s})
	return line
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
	stored := ""
	if persist {
		stored = s
	}
	h.AddStored(s, stored)
}

// AddStored records s for this session and writes stored to disk, when it
// is not empty: a prompt with a large paste is kept whole for Up in this
// session and written as the prompt with its placeholder.
func (h *History) AddStored(s, stored string) {
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
	if strings.TrimSpace(stored) == "" || h.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|noFollow, 0o600) // #nosec G304 -- the user's own history under their home
	if err != nil {
		return
	}
	defer f.Close()
	_ = f.Chmod(0o600) // a file made before, with a wider mode, is narrowed
	_, _ = f.Write(append(h.encode(stored), '\n'))
}
