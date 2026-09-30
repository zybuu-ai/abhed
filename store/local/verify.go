package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// scanned is a session file split into its complete lines and a torn tail.
type scanned struct {
	raws [][]byte
	// tail is what follows the last newline: a line a crash cut short.
	tail []byte
	// offsets[i] is where line i starts in the file.
	offsets []int64
}

func scan(data []byte) scanned {
	var s scanned
	var at int64
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			s.tail = data
			break
		}
		s.raws = append(s.raws, data[:i])
		s.offsets = append(s.offsets, at)
		at += int64(i) + 1
		data = data[i+1:]
	}
	return s
}

// verifyLines walks a chain of complete lines and reports the first that
// fails, with the hash of the last line checked. session is the id every line
// must carry, "" for any one id.
func verifyLines(raws [][]byte, session string) (Report, []line) {
	rep := Report{ID: session, OK: true}
	prev := Genesis
	var lastSeq int64
	seen := map[int64]bool{}
	out := make([]line, 0, len(raws))
	fail := func(i int, l line, reason string) (Report, []line) {
		rep.OK = false
		rep.Line = i + 1
		rep.FirstBad = l.Seq
		if rep.FirstBad == 0 {
			rep.FirstBad = lastSeq + 1
		}
		rep.EventID = l.ID
		rep.Reason = reason
		return rep, out
	}
	for i, raw := range raws {
		l, why := checkLine(raw)
		if why != "" {
			return fail(i, l, why)
		}
		if session == "" {
			session = l.SessionID
			rep.ID = session
		}
		switch {
		case l.SessionID != session:
			return fail(i, l, fmt.Sprintf("it belongs to session %s, not %s", l.SessionID, session))
		case l.Prev != prev:
			return fail(i, l, "it does not follow the line before it: a line was removed, added or moved")
		case l.Seq <= 0 || seen[l.Seq]:
			return fail(i, l, fmt.Sprintf("its seq %d is not a new step", l.Seq))
		}
		seen[l.Seq] = true
		prev, lastSeq = l.Hash, l.Seq
		out = append(out, l)
		rep.Events++
		rep.Head = Head{Lines: rep.Events, Seq: l.Seq, Hash: l.Hash}
	}
	return rep, out
}

// checkHead compares a head against the lines it should name, by count and
// hash. A head behind the last line is a sync point a crash left behind, and
// is noted; one past it, or naming another line, means lines went missing
// or were changed.
func checkHead(rep *Report, lines []line, head Head, have bool) {
	if !rep.OK {
		return
	}
	n := int64(len(lines))
	switch {
	case !have:
		if n > 0 {
			rep.OK, rep.Reason = false, "the head file is missing, so lines missing from the end cannot be ruled out"
			rep.FirstBad = rep.Head.Seq
		}
	case head.Lines > n:
		rep.OK = false
		rep.FirstBad = rep.Head.Seq + 1
		if n == 0 {
			rep.FirstBad = 1
			rep.Reason = fmt.Sprintf("the record is empty, but its head says it held %d lines", head.Lines)
			return
		}
		rep.Reason = fmt.Sprintf("lines are missing from the end: the head says the record held %d lines, it holds %d", head.Lines, n)
	case head.Lines > 0 && lines[head.Lines-1].Hash != head.Hash:
		l := lines[head.Lines-1]
		rep.OK, rep.FirstBad, rep.EventID, rep.Line = false, l.Seq, l.ID, int(head.Lines)
		rep.Reason = fmt.Sprintf("line %d is not the one the head recorded", head.Lines)
	case head.Lines < n:
		rep.Notes = append(rep.Notes, fmt.Sprintf("%d line(s) after the last sync point", n-head.Lines))
	}
}

// exportTrailer is the last line of an exported session: the head, so a copy
// can be checked away from the machine that wrote it.
type exportTrailer struct {
	Head    Head   `json:"abhed_record_head"`
	Session string `json:"session_id"`
	Format  int    `json:"format"`
	// Verified is whether the record verified when it was exported, and
	// Unverified why not.
	Verified   *bool  `json:"verified,omitempty"`
	Unverified string `json:"unverified,omitempty"`
}

// VerifyFile checks a session file on its own: a file in a records
// directory, or an exported copy. An export's trailer is its head; a file
// with neither a trailer nor a head beside it is checked line by line and
// noted, since lines cut from its end cannot then be detected.
func VerifyFile(path string) (Report, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the person names the file to check
	if err != nil {
		return Report{}, err
	}
	return verifyData(data, ""), nil
}

// verifyData verifies a file's contents on their own, with the head an
// export's trailer carries when it has one.
func verifyData(data []byte, session string) Report {
	s := scan(data)
	raws := s.raws
	var trailer *exportTrailer
	if n := len(raws); n > 0 {
		var t exportTrailer
		dec := json.NewDecoder(bytes.NewReader(raws[n-1]))
		dec.DisallowUnknownFields()
		if dec.Decode(&t) == nil && t.Format > 0 {
			trailer, raws = &t, raws[:n-1]
		}
	}
	rep, lines := verifyLines(raws, session)
	if len(s.tail) > 0 {
		rep.Torn = int64(len(s.tail))
		rep.Notes = append(rep.Notes, fmt.Sprintf("an unfinished last line of %d bytes, which a crash leaves; it is cut off when the session is next opened", len(s.tail)))
	}
	switch {
	case trailer != nil:
		if rep.OK && trailer.Session != rep.ID {
			rep.OK, rep.Reason = false, fmt.Sprintf("the trailer names session %s, not %s", trailer.Session, rep.ID)
		}
		if rep.OK && trailer.Verified != nil && !*trailer.Verified {
			rep.OK, rep.FirstBad = false, rep.Head.Seq
			rep.Reason = "it was exported from a record that failed verification: " + trailer.Unverified
		}
		checkHead(&rep, lines, trailer.Head, true)
	default:
		rep.Notes = append(rep.Notes, "no head: lines removed from the end would not show")
	}
	return rep
}

// Verify checks session id end to end: every line sealed and linked to the
// one before, the head file naming a line the record holds, and every head
// the index recorded at the session's ends still found in it.
func (s *Store) Verify(id string) (Report, error) {
	if err := checkID("session", id); err != nil {
		return Report{}, err
	}
	e, indexed := s.index.get(id)
	if indexed && e.Pruned {
		return Report{ID: id}, fmt.Errorf("session %s was pruned; its tombstone keeps head seq %d %s: %w", id, e.Head.Seq, e.Head.Hash, ErrNotFound)
	}
	s.mu.Lock()
	h := s.held[id]
	s.mu.Unlock()
	if h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
		_ = h.sync(s)
	}
	data, err := os.ReadFile(s.Path(id))
	if errors.Is(err, os.ErrNotExist) {
		if !indexed {
			return Report{ID: id}, fmt.Errorf("session %s: %w", id, ErrNotFound)
		}
		return Report{ID: id, Reason: "the session's file is missing, and no prune was recorded"}, nil
	}
	if err != nil {
		return Report{ID: id}, err
	}
	sc := scan(data)
	rep, lines := verifyLines(sc.raws, id)
	if len(sc.tail) > 0 {
		rep.Torn = int64(len(sc.tail))
		rep.Notes = append(rep.Notes, fmt.Sprintf("an unfinished last line of %d bytes, which a crash leaves; it is cut off when the session is next opened", len(sc.tail)))
	}
	head, have := s.readHead(id)
	checkHead(&rep, lines, head, have)
	if rep.OK {
		s.checkIndexHeads(&rep, id, lines)
	}
	return rep, nil
}

// checkIndexHeads compares the heads the index kept at each end of the
// session with its lines: a record cut back and given a new head file still
// disagrees with the index, which is chained on its own.
func (s *Store) checkIndexHeads(rep *Report, id string, lines []line) {
	ls, err := s.index.lines()
	if err != nil {
		return
	}
	for _, l := range ls {
		if l.ID != id || l.Op != opEnd || l.HeadLines == 0 {
			continue
		}
		if l.HeadLines > int64(len(lines)) {
			rep.OK, rep.FirstBad = false, rep.Head.Seq+1
			rep.Reason = fmt.Sprintf("lines are missing: the index recorded %d lines at an end, the record holds %d", l.HeadLines, len(lines))
			return
		}
		if got := lines[l.HeadLines-1]; got.Hash != l.HeadHash {
			rep.OK, rep.FirstBad, rep.EventID, rep.Line = false, got.Seq, got.ID, int(l.HeadLines)
			rep.Reason = fmt.Sprintf("line %d is not the one the index recorded at an end", l.HeadLines)
			return
		}
	}
}

// VerifyIndex checks the tenant's index: every line sealed, numbered and
// linked to the one before, and its head file naming a line it holds.
func (s *Store) VerifyIndex() (Report, error) {
	rep := Report{ID: "index", OK: true}
	data, err := os.ReadFile(s.index.path())
	if errors.Is(err, os.ErrNotExist) {
		return rep, nil
	}
	if err != nil {
		return rep, err
	}
	sc := scan(data)
	prev := Genesis
	for i, raw := range sc.raws {
		l, err := parseIndexLine(raw)
		bad := ""
		switch {
		case err != nil:
			bad = "the line is not an index entry: " + err.Error()
		case l.N != int64(i+1):
			bad = fmt.Sprintf("it is numbered %d at line %d: a line was removed, added or moved", l.N, i+1)
		case l.Prev != prev:
			bad = "it does not follow the line before it: a line was removed, added or moved"
		default:
			again := l
			if sealed, err := again.seal(); err != nil || again.Hash != l.Hash || !bytes.Equal(sealed, raw) {
				bad = "its hash does not match its content"
			}
		}
		if bad != "" {
			rep.OK, rep.Line, rep.FirstBad, rep.EventID, rep.Reason = false, i+1, int64(i+1), l.ID, bad
			return rep, nil
		}
		prev = l.Hash
		rep.Events++
		rep.Head = Head{Lines: l.N, Hash: l.Hash}
	}
	if len(sc.tail) > 0 {
		rep.Torn = int64(len(sc.tail))
		rep.Notes = append(rep.Notes, fmt.Sprintf("an unfinished last line of %d bytes, cut off at the next write", len(sc.tail)))
	}
	head, have := readHeadFile(s.index.headPath())
	switch {
	case !have && rep.Events > 0:
		rep.OK, rep.Reason = false, "the index head file is missing"
	case have && head.Lines > rep.Events:
		rep.OK, rep.FirstBad = false, rep.Events+1
		rep.Reason = fmt.Sprintf("index lines are missing: its head says it reached line %d", head.Lines)
	case have && head.Lines > 0 && head.Lines <= rep.Events:
		if l, _ := parseIndexLine(sc.raws[head.Lines-1]); l.Hash != head.Hash {
			rep.OK, rep.FirstBad, rep.Line = false, head.Lines, int(head.Lines)
			rep.Reason = fmt.Sprintf("index line %d is not the one its head recorded", head.Lines)
		}
	}
	return rep, nil
}

// VerifyAll checks the index and every session it lists, pruned ones aside.
func (s *Store) VerifyAll() (Report, []Report, error) {
	idx, err := s.VerifyIndex()
	if err != nil {
		return idx, nil, err
	}
	entries, err := s.index.entries()
	if err != nil {
		return idx, nil, err
	}
	var out []Report
	for _, e := range entries {
		if e.Pruned {
			continue
		}
		rep, err := s.Verify(e.ID)
		if err != nil {
			rep = Report{ID: e.ID, Reason: err.Error()}
		}
		out = append(out, rep)
	}
	return idx, out, nil
}
