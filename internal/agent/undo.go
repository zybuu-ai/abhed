package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Checkpoint is a file's content immediately before the agent changed it.
//
// Existed distinguishes "the file had this content" from "the file did not
// exist", because undoing a creation means deleting the file, not writing an
// empty one — a distinction that is easy to lose and produces confusing results
// when it is.
type Checkpoint struct {
	Path    string
	Before  []byte
	Existed bool
	Turn    int
	At      time.Time
	// Seq is the checkpoint.saved event that recorded it, and Blob the
	// sha256 its content is kept under; both zero when it was not recorded.
	Seq  int64
	Blob string
	// Mode is the file's permission bits when the checkpoint was taken.
	Mode os.FileMode
	// Skipped says why no content was kept; such a file is not restored.
	Skipped string
	// load reads Before from the record, for a checkpoint rebuilt on resume.
	load func() ([]byte, error)
}

// content is what the file held, read from the record when it was rebuilt.
func (cp Checkpoint) content() ([]byte, error) {
	if cp.Before != nil || cp.load == nil || !cp.Existed {
		return cp.Before, nil
	}
	return cp.load()
}

// UndoLog records checkpoints so a session's changes can be reverted.
//
// Per docs §09 this is the feature that makes users willing to let an agent
// edit their files at all: the cost of a wrong edit drops from "restore from
// git, if you committed" to one command.
type UndoLog struct {
	mu    sync.Mutex
	stack []Checkpoint
	turn  int
	// Persist, when set, also writes the checkpoint durably so undo survives a
	// restart, and returns it with its Seq and Blob. Failure to persist is
	// logged, not fatal — losing undo history is better than failing the
	// edit that was about to happen.
	Persist func(cp Checkpoint) (Checkpoint, error)
	// writeFile and removeFile restore and remove a file. They are the
	// session's, so undo is held to the workspace as the file tools are.
	writeFile  func(path string, data []byte) error
	removeFile func(path string) error
	// KeepFirst keeps only a path's earliest checkpoint, for a holder that
	// serves only Original, Accept and Changed: a long-lived server's session
	// then holds one copy per changed file, not one per edit.
	KeepFirst bool
}

// NewUndoLog records checkpoints that undo restores with write and removes
// with remove. Both are required: a session passes RestoreFile and
// RemoveFile, so a link swapped in cannot send undo out of the workspace.
func NewUndoLog(write func(path string, data []byte) error, remove func(path string) error) *UndoLog {
	return &UndoLog{writeFile: write, removeFile: remove}
}

// BeginTurn groups subsequent checkpoints, so undo reverts a whole turn's worth
// of edits rather than one file at a time. A model that edits four files to
// make one change should undo as one change.
func (u *UndoLog) BeginTurn() {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.turn++
	u.mu.Unlock()
}

// Record captures a file's prior state. Safe to call with a nil receiver so
// callers need not branch on whether undo is enabled.
func (u *UndoLog) Record(path string, before []byte, existed bool) {
	if u == nil {
		return
	}
	u.mu.Lock()
	if u.KeepFirst {
		for _, held := range u.stack {
			if held.Path == path {
				u.mu.Unlock()
				return
			}
		}
	}
	cp := Checkpoint{
		Path: path, Before: before, Existed: existed,
		Turn: u.turn, At: time.Now().UTC(),
	}
	u.stack = append(u.stack, cp)
	persist := u.Persist
	u.mu.Unlock()

	if persist == nil {
		return
	}
	saved, err := persist(cp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: checkpoint not persisted for %s: %v\n", path, err)
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := len(u.stack) - 1; i >= 0; i-- {
		if u.stack[i].Path == cp.Path && u.stack[i].At.Equal(cp.At) && u.stack[i].Seq == 0 {
			u.stack[i].Seq, u.stack[i].Blob, u.stack[i].Mode = saved.Seq, saved.Blob, saved.Mode
			if saved.Skipped != "" {
				// Not kept on disk, and not held here either.
				u.stack[i].Skipped, u.stack[i].Before = saved.Skipped, nil
			}
			break
		}
	}
}

// Rebuild replaces the log with the checkpoints a session's record holds, so
// undo and rewind work after a resume: each checkpoint.saved not yet used by
// a file.restored, its content read from get when it is needed.
func (u *UndoLog) Rebuild(events []Event, get func(sha string) ([]byte, error)) {
	if u == nil {
		return
	}
	var stack []Checkpoint
	turn := 0
	for _, ev := range events {
		switch ev.Type {
		case EvCheckpoint:
			var c CheckpointSaved
			if json.Unmarshal(ev.Payload, &c) != nil {
				continue
			}
			cp := Checkpoint{Path: c.Path, Existed: c.SHA256 != "" || c.Skipped != "", Turn: c.Turn, At: ev.CreatedAt, Seq: ev.Seq, Blob: c.SHA256, Mode: os.FileMode(c.Mode).Perm(), Skipped: c.Skipped}
			if sha := c.SHA256; sha != "" && get != nil {
				cp.load = func() ([]byte, error) { return get(sha) }
			}
			stack = append(stack, cp)
			turn = max(turn, c.Turn)
		case EvFileRestored:
			// A restore used every checkpoint from the one it names on.
			var r FileRestored
			if json.Unmarshal(ev.Payload, &r) != nil {
				continue
			}
			used, err := strconv.ParseInt(r.Checkpoint, 10, 64)
			if err != nil {
				continue
			}
			n := len(stack)
			for n > 0 && stack[n-1].Seq >= used {
				n--
			}
			stack = stack[:n]
		}
	}
	u.mu.Lock()
	u.stack, u.turn = stack, turn
	u.mu.Unlock()
}

// Since takes the checkpoints recorded after step seq off the log and
// returns, for each file, the earliest: what it held at that step. Paths
// are in order.
func (u *UndoLog) Since(seq int64) []Checkpoint {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	n := len(u.stack)
	for n > 0 && u.stack[n-1].Seq > seq {
		n--
	}
	taken := u.stack[n:]
	u.stack = u.stack[:n]
	earliest := map[string]Checkpoint{}
	var paths []string
	for _, cp := range taken {
		if _, ok := earliest[cp.Path]; !ok {
			earliest[cp.Path] = cp
			paths = append(paths, cp.Path)
		}
	}
	sort.Strings(paths)
	out := make([]Checkpoint, 0, len(paths))
	for _, p := range paths {
		out = append(out, earliest[p])
	}
	return out
}

// Peek is Since without taking anything off the log.
func (u *UndoLog) Peek(seq int64) int {
	if u == nil {
		return 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, cp := range u.stack {
		if cp.Seq > seq {
			n++
		}
	}
	return n
}

// LastTurnStart is the step just before the last turn with checkpoints on
// the log, for /undo; false when its checkpoints were not recorded.
func (u *UndoLog) LastTurnStart() (int64, bool) {
	if u == nil {
		return 0, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.stack) == 0 {
		return 0, false
	}
	last := u.stack[len(u.stack)-1].Turn
	first := int64(0)
	for i := len(u.stack) - 1; i >= 0 && u.stack[i].Turn == last; i-- {
		if u.stack[i].Seq == 0 {
			return 0, false
		}
		first = u.stack[i].Seq
	}
	return first - 1, true
}

// Undo reverts the most recent turn's changes and reports what it did.
func (u *UndoLog) Undo() ([]string, error) {
	if u == nil {
		return nil, fmt.Errorf("undo is not enabled for this session")
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	if len(u.stack) == 0 {
		return nil, fmt.Errorf("nothing to undo")
	}

	// Take every checkpoint from the most recent turn that recorded one.
	lastTurn := u.stack[len(u.stack)-1].Turn
	var batch []Checkpoint
	for len(u.stack) > 0 && u.stack[len(u.stack)-1].Turn == lastTurn {
		batch = append(batch, u.stack[len(u.stack)-1])
		u.stack = u.stack[:len(u.stack)-1]
	}

	// Within a turn a file may have been edited several times; only the EARLIEST
	// checkpoint restores the pre-turn state. batch is newest-first, so the last
	// entry per path is the one to apply.
	earliest := map[string]Checkpoint{}
	for _, cp := range batch {
		earliest[cp.Path] = cp
	}

	var restored []string
	var failures []string
	paths := make([]string, 0, len(earliest))
	for p := range earliest {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		cp := earliest[path]
		if cp.Skipped != "" {
			failures = append(failures, fmt.Sprintf("%s: no copy was kept before the change (%s)", path, cp.Skipped))
			continue
		}
		if !cp.Existed {
			// Undoing a creation means removing the file.
			if err := u.remove(path); err != nil && !os.IsNotExist(err) {
				failures = append(failures, fmt.Sprintf("%s: %v", path, err))
				continue
			}
			restored = append(restored, "removed "+filepath.Base(path))
			continue
		}
		// A snapshot holds whatever the file held, secrets included: owner-only.
		before, err := cp.content()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if err := u.write(path, before); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		restored = append(restored, "restored "+filepath.Base(path))
	}

	if len(failures) > 0 {
		return restored, fmt.Errorf("some files could not be reverted: %s",
			strings.Join(failures, "; "))
	}
	return restored, nil
}

func (u *UndoLog) write(path string, data []byte) error {
	if u.writeFile == nil {
		return fmt.Errorf("undo has no way to write files")
	}
	return u.writeFile(path, data)
}

func (u *UndoLog) remove(path string) error {
	if u.removeFile == nil {
		return fmt.Errorf("undo has no way to remove files")
	}
	return u.removeFile(path)
}

// Pending reports how many turns can still be undone.
func (u *UndoLog) Pending() int {
	if u == nil {
		return 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	turns := map[int]bool{}
	for _, cp := range u.stack {
		turns[cp.Turn] = true
	}
	return len(turns)
}

// Checkpoints are the checkpoints held, oldest first, without taking them.
func (u *UndoLog) Checkpoints() []Checkpoint {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Checkpoint(nil), u.stack...)
}

// Changed lists the files this session has modified, backing /diff.
func (u *UndoLog) Changed() []string {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, cp := range u.stack {
		if !seen[cp.Path] {
			seen[cp.Path] = true
			out = append(out, cp.Path)
		}
	}
	sort.Strings(out)
	return out
}

// Original returns the earliest recorded content for a path, so /diff can show
// the whole session's change rather than just the last edit.
// Accept moves a file's baseline to content: the change up to here has been
// reviewed and kept, so the changes view stops showing it and undo returns
// to it rather than to what came before.
func (u *UndoLog) Accept(path string, content []byte) bool {
	if u == nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := range u.stack {
		if u.stack[i].Path == path {
			u.stack[i].Before, u.stack[i].Existed = content, true
			return true
		}
	}
	return false
}

func (u *UndoLog) Original(path string) ([]byte, bool, bool) {
	if u == nil {
		return nil, false, false
	}
	u.mu.Lock()
	var found *Checkpoint
	for i := range u.stack {
		if u.stack[i].Path == path {
			cp := u.stack[i]
			found = &cp
			break
		}
	}
	u.mu.Unlock()
	if found == nil {
		return nil, false, false
	}
	data, err := found.content()
	if err != nil {
		return nil, found.Existed, false
	}
	return data, found.Existed, true
}
