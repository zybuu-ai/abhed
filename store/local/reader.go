package local

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// LineHash is the hash the record gives ev's line when prev is the line
// before: what a reader rebuilding a line from elsewhere, such as an OCSF
// event, checks against the hash it carries.
func LineHash(ev agent.Event, prev string) (string, error) {
	canon, err := canonical(ev.Payload)
	if err != nil {
		return "", err
	}
	ev.Payload = canon
	l := lineFor(ev, prev)
	if _, err := l.seal(); err != nil {
		return "", err
	}
	return l.Hash, nil
}

// SessionIDs lists every session the record holds, subagents' own included
// and pruned ones left out, oldest first.
func (s *Store) SessionIDs() ([]string, error) {
	all, err := s.index.entries()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for _, e := range all {
		if !e.Pruned {
			out = append(out, e.ID)
		}
	}
	return out, nil
}

// LinesFrom returns a session's whole lines from byte offset on, in file
// order, each with End set, and the offset just past the last of them: what a
// reader that keeps its place reads next, without the lines before. An offset
// that is past the end, or not where a line starts (the file was pruned and
// recorded again), reads from the start. An unfinished last line is left out.
func (s *Store) LinesFrom(sessionID string, offset int64) ([]Sealed, int64, error) {
	if err := checkID("session", sessionID); err != nil {
		return nil, 0, nil //nolint:nilerr // no such session is an empty record, as in Since
	}
	f, err := openOwn(s.Path(sessionID), os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if offset < 0 || offset > info.Size() || !lineStart(f, offset) {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, err
	}
	sc := scan(data)
	out := make([]Sealed, 0, len(sc.raws))
	end := offset
	for i, raw := range sc.raws {
		l, err := parseLine(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("the record is damaged at byte %d; run abhed record verify: %w", offset+sc.offsets[i], err)
		}
		end = offset + sc.offsets[i] + int64(len(raw)) + 1
		out = append(out, Sealed{Event: l.event(), Prev: l.Prev, Hash: l.Hash, End: end})
	}
	return out, end, nil
}

// lineStart reports whether offset is where a line starts: the file's start,
// or just after a newline.
func lineStart(f *os.File, offset int64) bool {
	if offset == 0 {
		return true
	}
	b := make([]byte, 1)
	n, err := f.ReadAt(b, offset-1)
	return err == nil && n == 1 && b[0] == '\n'
}

// Size is the length of a session's file, 0 when there is none: a reader
// that kept the End of the last line it read has more to read when it grew.
func (s *Store) Size(sessionID string) (int64, error) {
	if err := checkID("session", sessionID); err != nil {
		return 0, nil //nolint:nilerr // no such session is an empty record
	}
	info, err := os.Stat(s.Path(sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
