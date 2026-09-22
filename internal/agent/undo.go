package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// restart. Failure to persist is logged, not fatal — losing undo history is
	// better than failing the edit that was about to happen.
	Persist func(cp Checkpoint) error
}

func NewUndoLog() *UndoLog { return &UndoLog{} }

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
	cp := Checkpoint{
		Path: path, Before: before, Existed: existed,
		Turn: u.turn, At: time.Now().UTC(),
	}
	u.stack = append(u.stack, cp)
	persist := u.Persist
	u.mu.Unlock()

	if persist != nil {
		if err := persist(cp); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: checkpoint not persisted for %s: %v\n", path, err)
		}
	}
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
		if !cp.Existed {
			// Undoing a creation means removing the file.
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				failures = append(failures, fmt.Sprintf("%s: %v", path, err))
				continue
			}
			restored = append(restored, "removed "+filepath.Base(path))
			continue
		}
		// A snapshot holds whatever the file held, secrets included: owner-only.
		if err := os.WriteFile(path, cp.Before, 0o600); err != nil {
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
	defer u.mu.Unlock()
	for _, cp := range u.stack {
		if cp.Path == path {
			return cp.Before, cp.Existed, true
		}
	}
	return nil, false, false
}
