package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/zybuu-ai/abhed/internal/secretfiles"
)

// Changes a bash command makes to the workspace are checkpointed as the file
// tools' are: before the command the workspace's files are noted, with the
// content of those within the limits; afterwards each file that changed, was
// made or was removed is handed to the session's BashCheckpoint. Only the
// changed files reach the record; the content is held in memory between calls
// and re-read only for files whose size or times changed.

// BashCheckpointMode says what happens to a command's changes.
type BashCheckpointMode string

const (
	// BashCheckpointOn snapshots before each command and says in the result
	// when it could not; the command runs either way. The default.
	BashCheckpointOn BashCheckpointMode = "on"
	// BashCheckpointOff takes no snapshot.
	BashCheckpointOff BashCheckpointMode = "off"
	// BashCheckpointRequired refuses a command when the snapshot before it
	// is not whole: a file over a limit, or a workspace over the file count.
	BashCheckpointRequired BashCheckpointMode = "required"
)

// Default bounds of a bash snapshot.
const (
	DefaultBashSnapshotBytes = 32 << 20
	DefaultBashSnapshotFiles = 20000
	bashSnapshotFileBytes    = 2 << 20
	// bashSnapshotChanges bounds the checkpoints one command records; a
	// command that changes more says so in its result.
	bashSnapshotChanges = 2000
)

// BashCheckpoints configures snapshots before bash commands. The zero value
// is on, with the default limits.
type BashCheckpoints struct {
	Mode BashCheckpointMode
	// MaxBytes bounds the content held; MaxFiles the files looked at.
	MaxBytes int64
	MaxFiles int
}

// BashCheckpointsFrom is tools.bash_checkpoints and its limits, as configured.
func BashCheckpointsFrom(mode string, mb, files int) BashCheckpoints {
	c := BashCheckpoints{Mode: BashCheckpointMode(mode), MaxBytes: int64(mb) << 20, MaxFiles: files}
	if c.Mode == "" {
		c.Mode = BashCheckpointOn
	}
	return c
}

func (c BashCheckpoints) maxBytes() int64 {
	if c.MaxBytes > 0 {
		return c.MaxBytes
	}
	return DefaultBashSnapshotBytes
}

func (c BashCheckpoints) maxFiles() int {
	if c.MaxFiles > 0 {
		return c.MaxFiles
	}
	return DefaultBashSnapshotFiles
}

// FileBefore is a file as it was before a command changed it: its content
// and permission bits, Existed false for one the command made, or Skipped
// saying why no content was kept.
type FileBefore struct {
	Path    string
	Before  []byte
	Existed bool
	Mode    os.FileMode
	Skipped string
}

// snapEntry is one file as a snapshot saw it.
type snapEntry struct {
	size  int64
	mtime int64
	ctime int64
	mode  os.FileMode
	data  []byte
	held  bool
	// why says why the content is not held.
	why string
}

func (e snapEntry) same(o snapEntry) bool {
	return e.size == o.size && e.mtime == o.mtime && e.ctime == o.ctime && e.mode == o.mode
}

// wsSnapshot is the workspace's files before a command.
type wsSnapshot struct {
	files map[string]snapEntry
	// cut says why the snapshot is not whole; "" when every file was looked at and held.
	cut string
	// partial is set when files were left out of the walk itself, which a
	// later walk then cannot compare.
	partial bool
}

// bashSnaps is a session's snapshot state, kept between calls.
type bashSnaps struct {
	mu   sync.Mutex
	last map[string]snapEntry
	// told is set once the result has said the walk was cut.
	told bool
}

func (s *Session) snaps() *bashSnaps {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bashSnaps == nil {
		s.bashSnaps = &bashSnaps{}
	}
	return s.bashSnaps
}

// bashSnapshotOn reports whether a command's changes are to be checkpointed.
func (s *Session) bashSnapshotOn() bool {
	return s.BashCheckpoint != nil && s.BashCheckpoints.Mode != BashCheckpointOff
}

// walkWorkspace notes the workspace's regular files, as glob and grep see
// it: the folders they pass over, links and Abhed's state are left out. With
// prev, content already held for a file that has not changed is kept rather
// than read again; read false reads nothing.
func (s *Session) walkWorkspace(ctx context.Context, prev map[string]snapEntry, read bool) wsSnapshot {
	cfg := s.BashCheckpoints
	snap := wsSnapshot{files: map[string]snapEntry{}}
	state := s.stateSet()
	root := s.Root
	budget := cfg.maxBytes()
	limit := cfg.maxFiles()
	var over []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is left out, as glob leaves it
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != root && (skipDirs[d.Name()] || state.HasEntry(d)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || state.HasEntry(d) {
			return nil
		}
		if len(snap.files) >= limit {
			snap.partial = true
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // gone since it was listed
		}
		e := snapEntry{size: info.Size(), mtime: info.ModTime().UnixNano(), ctime: changeTime(info), mode: info.Mode()}
		if !read {
			snap.files[path] = e
			return nil
		}
		if p, ok := prev[path]; ok && p.same(e) && p.held {
			e.data, e.held = p.data, true
			budget -= int64(len(e.data))
		} else {
			switch {
			case secretfiles.Match(path) != "":
				e.why = "it looks like a file of keys or credentials"
			case e.size > bashSnapshotFileBytes:
				e.why = fmt.Sprintf("it was over the %d MiB a command's checkpoint keeps of one file", bashSnapshotFileBytes>>20)
				over = append(over, path)
			case e.size > budget:
				e.why = fmt.Sprintf("the workspace's files were over the %d MiB a command's checkpoint keeps", cfg.maxBytes()>>20)
				over = append(over, path)
			default:
				data, err := s.readFile(path)
				if err != nil {
					e.why = "it could not be read: " + err.Error()
					break
				}
				e.data, e.held = data, true
				budget -= int64(len(data))
			}
		}
		snap.files[path] = e
		return nil
	})
	switch {
	case err != nil && !errors.Is(err, filepath.SkipAll):
		snap.cut = "the workspace could not be read: " + err.Error()
	case snap.partial:
		snap.cut = fmt.Sprintf("the workspace has more than the %d files a command's checkpoint looks at", limit)
	case len(over) > 0:
		sort.Strings(over)
		snap.cut = fmt.Sprintf("%d file(s) were over its limits, %s first", len(over), s.rel(over[0]))
	}
	return snap
}

// rel is path relative to the workspace, for a note.
func (s *Session) rel(path string) string {
	if r, err := filepath.Rel(s.Root, path); err == nil {
		return r
	}
	return path
}

// beforeBash snapshots the workspace before a command. It returns the
// snapshot, nil when none is taken, and a refusal when the mode requires a
// whole snapshot and this one is not.
func (s *Session) beforeBash(ctx context.Context) (*wsSnapshot, error) {
	if !s.bashSnapshotOn() {
		return nil, nil
	}
	sn := s.snaps()
	sn.mu.Lock()
	snap := s.walkWorkspace(ctx, sn.last, true)
	sn.last = snap.files
	sn.mu.Unlock()
	if snap.cut != "" && s.BashCheckpoints.Mode == BashCheckpointRequired {
		return nil, fmt.Errorf("refused: checkpoints.bash is \"required\" and the snapshot before this command is not whole: %s. Nothing was run", snap.cut)
	}
	return &snap, nil
}

// afterBash hands each file the command changed to BashCheckpoint, and
// returns a note for the result when the changes were not all kept.
func (s *Session) afterBash(snap *wsSnapshot) string {
	if snap == nil {
		return ""
	}
	now := s.walkWorkspace(context.Background(), nil, false)
	var changed []FileBefore
	for path, b := range snap.files {
		a, ok := now.files[path]
		if ok && a.same(b) {
			continue
		}
		if !ok && now.partial {
			continue // not looked at this time, which says nothing about it
		}
		fb := FileBefore{Path: path, Existed: true, Mode: b.mode.Perm()}
		if b.held {
			fb.Before = b.data
		} else {
			fb.Skipped = b.why
		}
		changed = append(changed, fb)
	}
	for path := range now.files {
		if _, ok := snap.files[path]; !ok && !snap.partial {
			changed = append(changed, FileBefore{Path: path})
		}
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].Path < changed[j].Path })
	var notes []string
	if len(changed) > bashSnapshotChanges {
		notes = append(notes, fmt.Sprintf("it changed %d files, more than the %d a command's checkpoint records; /undo takes back the first %d by name", len(changed), bashSnapshotChanges, bashSnapshotChanges))
		changed = changed[:bashSnapshotChanges]
	}
	skipped := 0
	for _, fb := range changed {
		if fb.Skipped != "" {
			skipped++
		}
		s.BashCheckpoint(fb)
	}
	if skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d changed file(s) had no copy kept before it", skipped))
	}
	if snap.cut != "" {
		sn := s.snaps()
		sn.mu.Lock()
		first := !sn.told
		sn.told = true
		sn.mu.Unlock()
		if first || skipped > 0 {
			notes = append(notes, "the snapshot before it was not whole: "+snap.cut)
		}
	}
	if len(notes) == 0 {
		return ""
	}
	out := "[checkpoint: "
	for i, n := range notes {
		if i > 0 {
			out += "; "
		}
		out += n
	}
	return out + ". /undo cannot take back what was not kept.]"
}
