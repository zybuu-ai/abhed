package local

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// SegmentLine is one line of a stretch of a session's record checked away
// from the machine that wrote it.
type SegmentLine struct {
	Seq       int64
	ID        string
	Type      string
	CreatedAt time.Time
	Prev      string
	Hash      string
	// Raw is the line byte for byte, without its newline.
	Raw []byte
}

// CheckSegment checks raws as consecutive sealed lines of session's record
// following the line hashed before (Genesis first), as a receiving server must.
func CheckSegment(raws [][]byte, session, before string) ([]SegmentLine, error) {
	if err := checkID("session", session); err != nil {
		return nil, err
	}
	if before != Genesis && !isHash(before) {
		return nil, errors.New("the hash the segment follows is not a sha256")
	}
	out := make([]SegmentLine, 0, len(raws))
	prev := before
	seen := map[int64]bool{}
	for i, raw := range raws {
		l, why := checkLine(raw)
		switch {
		case why != "":
		case l.SessionID != session:
			why = fmt.Sprintf("it belongs to session %s, not %s", l.SessionID, session)
		case l.Prev != prev:
			why = "it does not follow the line before it: a line was removed, added or moved"
		case l.Seq <= 0 || seen[l.Seq]:
			why = fmt.Sprintf("its seq %d is not a new step", l.Seq)
		}
		if why != "" {
			return out, fmt.Errorf("line %d of the segment: %s", i+1, why)
		}
		seen[l.Seq] = true
		at, _ := time.Parse(timeFormat, l.CreatedAt)
		out = append(out, SegmentLine{Seq: l.Seq, ID: l.ID, Type: l.Type, CreatedAt: at, Prev: l.Prev, Hash: l.Hash,
			Raw: append([]byte(nil), raw...)})
		prev = l.Hash
	}
	return out, nil
}

// WriteExport writes a session's whole lines in the export format, with a
// trailer of the head they verify to (or saying they do not).
func WriteExport(w io.Writer, session string, raws [][]byte) (Head, error) {
	if err := checkID("session", session); err != nil {
		return Head{}, err
	}
	rep, _ := verifyLines(raws, session)
	for _, raw := range raws {
		if _, err := w.Write(append(append([]byte(nil), raw...), '\n')); err != nil {
			return Head{}, err
		}
	}
	tr := exportTrailer{Head: rep.Head, Session: session, Format: ExportFormat, Verified: boolPtr(rep.OK)}
	if !rep.OK {
		tr.Unverified = fmt.Sprintf("at seq %d, %s", rep.FirstBad, rep.Reason)
	}
	t, err := encode(tr)
	if err != nil {
		return rep.Head, err
	}
	_, err = w.Write(append(t, '\n'))
	return rep.Head, err
}

// VerifyLines checks a session's whole lines held away from the machine that
// wrote them: every line sealed and linked to the one before.
func VerifyLines(raws [][]byte, session string) Report {
	rep, _ := verifyLines(raws, session)
	return rep
}
