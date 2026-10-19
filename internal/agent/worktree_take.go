package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/linediff"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Taking a worktree back: what a subagent left in its worktree is shown as
// hunks against the commit the worktree shares with the main tree, and the
// hunks a person accepts are put into the main tree as that person's own
// writes, through Loop.Manual, so policy, git's protection, checkpoints and
// the record apply as to any edit. A hunk the main tree has changed under
// is a conflict: reported, never forced.

// EvWorktreeTaken records a take: which worktree, what was put into the main
// tree, what conflicted, and whether the worktree was removed.
const EvWorktreeTaken EventType = "worktree.taken"

// WorktreeTaken is the payload of worktree.taken.
type WorktreeTaken struct {
	Worktree  string   `json:"worktree"`
	Branch    string   `json:"branch"`
	Base      string   `json:"base"`
	Taken     []string `json:"taken,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
	Refused   []string `json:"refused,omitempty"`
	Left      int      `json:"left"`
	Removed   bool     `json:"removed,omitempty"`
}

// takeMaxFile bounds the files compared by hunk; a larger one is taken whole or not.
const takeMaxFile = 4 << 20

// Worktree is a subagent's worktree that can be taken back.
type Worktree struct {
	ID     string `json:"id"`
	Dir    string `json:"-"`
	Rel    string `json:"rel"`
	Branch string `json:"branch"`
}

// WorktreeFile is one file a worktree changed, by the workspace's slash path.
type WorktreeFile struct {
	Path string `json:"path"`
	// Status is modified, added or deleted.
	Status string `json:"status"`
	// Whole is set for a file taken as one change: binary or too large.
	Whole bool `json:"whole,omitempty"`
	// Unsupported says why the file cannot be taken here, such as a link.
	Unsupported string         `json:"unsupported,omitempty"`
	Hunks       []WorktreeHunk `json:"hunks"`

	base, theirs       []byte
	hasBase, hasTheirs bool
}

// WorktreeHunk is one change in a file, with up to three lines of the base
// on each side for context.
type WorktreeHunk struct {
	OldStart int      `json:"old_start"`
	OldCount int      `json:"old_count"`
	NewStart int      `json:"new_start"`
	NewCount int      `json:"new_count"`
	Old      []string `json:"old"`
	New      []string `json:"new"`
	Before   []string `json:"before,omitempty"`
	After    []string `json:"after,omitempty"`
}

// WorktreeChanges is what a worktree holds against Base, the commit it shares
// with the main tree.
type WorktreeChanges struct {
	Worktree
	Base  string         `json:"base"`
	Files []WorktreeFile `json:"files"`
}

// ListWorktrees are the subagent worktrees under ws's .abhed-worktrees, on
// their abhed/ branches; ws must be the main tree.
func ListWorktrees(ctx context.Context, ws string) ([]Worktree, error) {
	if err := requireGitRepo(ctx, ws); err != nil {
		return nil, err
	}
	out, err := git(ctx, hostgit.New(ctx, ws), "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	parent := filepath.Join(tools.RealPath(ws), tools.WorktreesDir)
	var list []Worktree
	var dir, branch string
	flush := func() {
		if dir != "" && filepath.Dir(tools.RealPath(dir)) == parent && strings.HasPrefix(branch, "abhed/") {
			id := filepath.Base(dir)
			if branch == "abhed/"+id {
				list = append(list, Worktree{ID: id, Dir: filepath.Join(ws, tools.WorktreesDir, id),
					Rel: path.Join(tools.WorktreesDir, id), Branch: branch})
			}
		}
		dir, branch = "", ""
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			dir = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch refs/heads/"):
			branch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}
	flush()
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list, nil
}

// FindWorktree is the worktree id names, or an error naming those there are.
func FindWorktree(ctx context.Context, ws, id string) (Worktree, error) {
	list, err := ListWorktrees(ctx, ws)
	if err != nil {
		return Worktree{}, err
	}
	var ids []string
	for _, w := range list {
		if w.ID == id || w.Branch == id || w.Rel == strings.TrimSuffix(id, "/") {
			return w, nil
		}
		ids = append(ids, w.ID)
	}
	if len(ids) == 0 {
		return Worktree{}, fmt.Errorf("no subagent worktree %q: there are none", id)
	}
	return Worktree{}, fmt.Errorf("no subagent worktree %q; there are %s", id, strings.Join(ids, ", "))
}

// Changes lists what the worktree holds against the commit it shares with
// the main tree at ws: its commits and what it left uncommitted, untracked
// files included.
func (w Worktree) Changes(ctx context.Context, ws string) (WorktreeChanges, error) {
	main := hostgit.New(ctx, ws)
	base, err := git(ctx, main, "merge-base", "HEAD", "refs/heads/"+w.Branch)
	if err != nil {
		return WorktreeChanges{}, err
	}
	tree := hostgit.New(ctx, w.Dir)
	raw, err := gitRaw(ctx, tree, "diff", "--raw", "-z", "--no-renames", "--no-ext-diff", base, "--")
	if err != nil {
		return WorktreeChanges{}, err
	}
	type entry struct{ status, mode string }
	paths := map[string]entry{}
	f := strings.Split(raw, "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		meta := strings.Fields(f[i])
		if len(meta) < 5 {
			continue
		}
		paths[f[i+1]] = entry{status: meta[4], mode: meta[1]}
	}
	untracked, err := gitRaw(ctx, tree, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return WorktreeChanges{}, err
	}
	for _, p := range strings.Split(untracked, "\x00") {
		if p != "" {
			paths[p] = entry{status: "A"}
		}
	}
	root, err := os.OpenRoot(w.Dir)
	if err != nil {
		return WorktreeChanges{}, err
	}
	defer func() { _ = root.Close() }()
	out := WorktreeChanges{Worktree: w, Base: base}
	names := make([]string, 0, len(paths))
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		e := paths[p]
		wf := WorktreeFile{Path: p, Status: "modified"}
		switch e.status {
		case "A":
			wf.Status = "added"
		case "D":
			wf.Status = "deleted"
		}
		if why := takeRefusedPath(p); why != "" {
			wf.Unsupported = why
			out.Files = append(out.Files, wf)
			continue
		}
		if wf.Status != "added" {
			if b, err := gitRaw(ctx, main, "cat-file", "blob", base+":"+p); err == nil {
				wf.base, wf.hasBase = []byte(b), true
			}
		}
		if wf.Status != "deleted" {
			info, err := root.Lstat(filepath.FromSlash(p))
			switch {
			case err != nil:
				wf.Unsupported = "it cannot be read in the worktree"
			case info.Mode()&fs.ModeSymlink != 0:
				wf.Unsupported = "it is a link; take it by hand"
			case !info.Mode().IsRegular():
				wf.Unsupported = "it is not a regular file (a submodule?); take it by hand"
			case info.Size() > takeMaxFile:
				wf.Whole = true
				fallthrough
			default:
				if wf.Unsupported == "" {
					data, err := root.ReadFile(filepath.FromSlash(p))
					if err != nil {
						wf.Unsupported = "it cannot be read in the worktree"
						break
					}
					wf.theirs, wf.hasTheirs = data, true
				}
			}
		}
		if e.mode == "120000" || e.mode == "160000" {
			wf.Unsupported = "it is a link or a submodule; take it by hand"
		}
		if wf.Unsupported == "" {
			if tools.IsBinary(wf.base) || tools.IsBinary(wf.theirs) || len(wf.base) > takeMaxFile {
				wf.Whole = true
			}
			wf.Hunks = fileHunks(wf)
			if len(wf.Hunks) == 0 && wf.hasBase == wf.hasTheirs {
				continue // a mode change alone, or nothing
			}
		}
		out.Files = append(out.Files, wf)
	}
	return out, nil
}

// takeRefusedPath says why a path is never taken into the main tree: git's
// own folders and Abhed's state are not the subagent's to change.
func takeRefusedPath(p string) string {
	for _, seg := range strings.Split(p, "/") {
		switch strings.ToLower(seg) {
		case ".git":
			return "it is in a .git folder"
		case strings.ToLower(tools.StateDir):
			return "it is in Abhed's state folder"
		case strings.ToLower(tools.WorktreesDir):
			return "it is in the worktrees folder"
		}
	}
	return ""
}

// fileHunks are the file's changes: one for a file taken whole.
func fileHunks(f WorktreeFile) []WorktreeHunk {
	if f.Whole {
		return []WorktreeHunk{{}}
	}
	a, b := linediff.SplitLines(string(f.base)), linediff.SplitLines(string(f.theirs))
	hs := linediff.Hunks(a, b)
	out := make([]WorktreeHunk, len(hs))
	for i, h := range hs {
		out[i] = WorktreeHunk{OldStart: h.OldStart, OldCount: h.OldCount, NewStart: h.NewStart, NewCount: h.NewCount,
			Old: a[h.OldStart : h.OldStart+h.OldCount], New: b[h.NewStart : h.NewStart+h.NewCount],
			Before: a[max(0, h.OldStart-3):h.OldStart], After: a[h.OldStart+h.OldCount : min(len(a), h.OldStart+h.OldCount+3)]}
	}
	return out
}

// TakeWriter puts a file into the main tree as the person's own change, or
// removes one; rel is the workspace's slash path.
type TakeWriter interface {
	WriteFile(ctx context.Context, rel string, content []byte) error
	RemoveFile(ctx context.Context, rel string) error
	// ReadFile is the main tree's file, ok false when there is none.
	ReadFile(rel string) (data []byte, ok bool, err error)
}

// TakeResult says what a take did, file by file.
type TakeResult struct {
	Taken     []string `json:"taken"`
	Conflicts []string `json:"conflicts"`
	Refused   []string `json:"refused"`
	// Left counts the hunks not taken: not accepted, or in conflict.
	Left    int  `json:"left"`
	Removed bool `json:"removed"`
}

// Take puts the accepted hunks of each file (by index, as Changes lists them)
// into the main tree through tw, and records worktree.taken. With remove, the
// worktree and its branch are removed afterwards, as the person's delete of
// it; what was not taken goes with it. The changes are read again here, so a
// listing made earlier cannot put a stale hunk in.
func (l *Loop) Take(ctx context.Context, ws string, w Worktree, accept map[string][]int, remove bool, tw TakeWriter) (TakeResult, error) {
	ch, err := w.Changes(ctx, ws)
	if err != nil {
		return TakeResult{}, err
	}
	var res TakeResult
	known := map[string]bool{}
	for _, f := range ch.Files {
		known[f.Path] = true
		picked := distinct(accept[f.Path])
		n := len(f.Hunks)
		if f.Unsupported != "" {
			n = 1
		}
		res.Left += n - min(n, len(picked))
		if len(picked) == 0 {
			continue
		}
		if f.Unsupported != "" {
			res.Refused = append(res.Refused, f.Path+": "+f.Unsupported)
			continue
		}
		next, gone, conflicts, err := mergeTake(f, picked, tw)
		res.Left += conflicts
		switch {
		case err != nil:
			res.Refused = append(res.Refused, f.Path+": "+err.Error())
			res.Left += len(picked) - conflicts
			continue
		case conflicts > 0:
			res.Conflicts = append(res.Conflicts, fmt.Sprintf("%s: %d hunk(s) conflict with the main tree's own change there", f.Path, conflicts))
		}
		if next == nil && !gone {
			continue // nothing left to change
		}
		if gone {
			err = tw.RemoveFile(ctx, f.Path)
		} else {
			err = tw.WriteFile(ctx, f.Path, next)
		}
		if err != nil {
			res.Refused = append(res.Refused, f.Path+": "+err.Error())
			res.Left += len(picked) - conflicts
			continue
		}
		res.Taken = append(res.Taken, f.Path)
	}
	for p := range accept {
		if !known[p] {
			res.Refused = append(res.Refused, p+": the worktree no longer changes it")
		}
	}
	sort.Strings(res.Refused)
	if remove {
		res.Removed = l.removeTaken(ctx, ws, w)
	}
	l.record(EvWorktreeTaken, ActorUser, WorktreeTaken{Worktree: w.Rel, Branch: w.Branch, Base: ch.Base,
		Taken: res.Taken, Conflicts: res.Conflicts, Refused: res.Refused, Left: res.Left, Removed: res.Removed})
	return res, nil
}

// removeTaken removes the worktree and its branch as the person's delete of
// the folder, which a delete rule can refuse.
func (l *Loop) removeTaken(ctx context.Context, ws string, w Worktree) bool {
	id := "u" + newID()
	args, _ := json.Marshal(map[string]string{"path": w.Dir})
	refused, err := l.ManualCheck("delete", id, args)
	if err != nil || refused != nil {
		if refused != nil {
			_ = l.ManualObserve(id, "delete", *refused, 0)
		}
		return false
	}
	start := time.Now()
	removeWorktree(context.WithoutCancel(ctx), ws, &worktree{Dir: w.Dir, Branch: w.Branch})
	_, statErr := os.Lstat(w.Dir)
	gone := errors.Is(statErr, fs.ErrNotExist)
	result := tools.Result{Content: "removed the worktree " + w.Rel + " and its branch " + w.Branch}
	if !gone {
		result = tools.Result{Content: "the worktree " + w.Rel + " could not be removed", IsError: true}
	}
	_ = l.ManualObserve(id, "delete", result, time.Since(start))
	return gone
}

// mergeTake is the file's content with the picked hunks put in, against the
// main tree as it is now: nil when nothing changes, gone when the file is to
// be removed. A hunk the main tree changed at, or next to, is a conflict and
// left out.
func mergeTake(f WorktreeFile, picked []int, tw TakeWriter) (next []byte, gone bool, conflicts int, err error) {
	for _, i := range picked {
		if i < 0 || i >= len(f.Hunks) {
			return nil, false, 0, fmt.Errorf("no hunk %d", i)
		}
	}
	cur, exists, err := tw.ReadFile(f.Path)
	if err != nil {
		return nil, false, 0, err
	}
	same := func(data []byte, ok bool, want []byte, wantOK bool) bool {
		return ok == wantOK && (!ok || bytes.Equal(data, want))
	}
	if f.Whole {
		switch {
		case same(cur, exists, f.theirs, f.hasTheirs):
			return nil, false, 0, nil // already there
		case !same(cur, exists, f.base, f.hasBase):
			return nil, false, 1, nil
		case !f.hasTheirs:
			return nil, true, 0, nil
		}
		return f.theirs, false, 0, nil
	}
	if f.hasBase != exists {
		// Removed, or made, in the main tree meanwhile.
		if same(cur, exists, f.theirs, f.hasTheirs) {
			return nil, false, 0, nil
		}
		return nil, false, len(picked), nil
	}
	base := linediff.SplitLines(string(f.base))
	mine := linediff.SplitLines(string(cur))
	theirs := linediff.SplitLines(string(f.theirs))
	var own []linediff.Hunk
	if !bytes.Equal(cur, f.base) {
		own = linediff.Hunks(base, mine)
	}
	sorted := append([]int(nil), picked...)
	sort.Sort(sort.Reverse(sort.IntSlice(sorted)))
	out := append([]string(nil), mine...)
	applied := 0
	for _, i := range sorted {
		h := f.Hunks[i]
		lo, hi := h.OldStart, h.OldStart+h.OldCount
		offset, clash, already := 0, false, false
		for _, o := range own {
			olo, ohi := o.OldStart, o.OldStart+o.OldCount
			if olo == lo && ohi == hi && equalLines(mine[o.NewStart:o.NewStart+o.NewCount], theirs[h.NewStart:h.NewStart+h.NewCount]) {
				already = true
				break
			}
			if lo <= ohi && olo <= hi {
				clash = true
				break
			}
			if ohi <= lo {
				offset += o.NewCount - o.OldCount
			}
		}
		if already {
			continue
		}
		at := lo + offset
		if clash || at < 0 || at+h.OldCount > len(out) || !equalLines(out[at:at+h.OldCount], base[lo:hi]) {
			conflicts++
			continue
		}
		repl := theirs[h.NewStart : h.NewStart+h.NewCount]
		out = append(out[:at], append(append([]string(nil), repl...), out[at+h.OldCount:]...)...)
		applied++
	}
	if applied == 0 {
		return nil, false, conflicts, nil
	}
	text := strings.Join(out, "")
	if !f.hasTheirs && text == "" && conflicts == 0 {
		return nil, true, 0, nil
	}
	return []byte(text), false, conflicts, nil
}

// distinct is the hunk indices once each, in order.
func distinct(in []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, i := range in {
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// gitRaw is git with its output as it is, for -z lists and file contents.
func gitRaw(ctx context.Context, r *hostgit.Repo, args ...string) (string, error) {
	cmd := r.Command(ctx, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out.String(), nil
}

// ManualTaker is the TakeWriter for a session's person: each write is the
// write tool through Loop.Manual on a fork of sess, each removal a judged and
// recorded delete, so policy, git's protection and checkpoints hold.
func (l *Loop) ManualTaker(sess *tools.Session) TakeWriter {
	return manualTaker{l: l, sess: sess}
}

type manualTaker struct {
	l    *Loop
	sess *tools.Session
}

func (m manualTaker) abs(rel string) string {
	return filepath.Join(m.sess.Root, filepath.FromSlash(rel))
}

func (m manualTaker) ReadFile(rel string) ([]byte, bool, error) {
	data, err := m.sess.ReadFile(m.abs(rel))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func (m manualTaker) WriteFile(ctx context.Context, rel string, content []byte) error {
	s := m.sess.Fork()
	if s.Syntax == tools.SyntaxRefuse {
		s.Syntax = tools.SyntaxReport // the person's take is warned about, never refused
	}
	p := m.abs(rel)
	if data, err := s.ReadFile(p); err == nil {
		s.MarkRead(p, string(data))
	}
	args, _ := json.Marshal(map[string]string{"path": p, "content": string(content)})
	res, err := m.l.Manual(ctx, s, "write", "u"+newID(), args)
	if err != nil {
		return err
	}
	if res.IsError {
		return errors.New(res.Content)
	}
	return nil
}

func (m manualTaker) RemoveFile(_ context.Context, rel string) error {
	p, id := m.abs(rel), "u"+newID()
	args, _ := json.Marshal(map[string]string{"path": p})
	refused, err := m.l.ManualCheck("delete", id, args)
	if err != nil {
		return err
	}
	if refused != nil {
		_ = m.l.ManualObserve(id, "delete", *refused, 0)
		return errors.New(refused.Content)
	}
	if m.sess.Checkpoint != nil {
		if data, err := m.sess.ReadFile(p); err == nil {
			m.sess.Checkpoint(p, data, true)
		}
	}
	start := time.Now()
	if err := m.sess.RemoveFile(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = m.l.ManualObserve(id, "delete", tools.Result{Content: err.Error(), IsError: true}, time.Since(start))
		return err
	}
	return m.l.ManualObserve(id, "delete", tools.Result{Content: "removed " + p}, time.Since(start))
}
