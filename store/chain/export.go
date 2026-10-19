package chain

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"
)

// ExportVersion is the version of the export format Export writes.
const ExportVersion = 1

// Header is an export's first line: which chain it is, and where it starts.
type Header struct {
	V      int    `json:"abhed_chain"`
	Chain  string `json:"chain"`
	Tenant string `json:"tenant"`
	Scope  string `json:"scope"`
	// From is the first position in the file, and Before the hash it links to.
	From   int64  `json:"from"`
	Before string `json:"before"`
	// PreChain counts rows written before the chain existed, which no hash
	// covers and the file does not hold.
	PreChain   int64     `json:"pre_chain,omitempty"`
	ExportedAt time.Time `json:"exported_at"`
}

// trailer is an export's last line: the head the store gave when it was written.
type trailer struct {
	Head *Head `json:"head"`
}

// Writer writes an export: a header, one link per line, and a trailer.
type Writer struct {
	w   *bufio.Writer
	err error
}

// NewWriter writes h as the export's first line.
func NewWriter(w io.Writer, h Header) *Writer {
	h.V = ExportVersion
	if h.Before == "" {
		h.Before = Genesis
	}
	ew := &Writer{w: bufio.NewWriter(w)}
	ew.line(h)
	return ew
}

func (e *Writer) line(v any) {
	if e.err != nil {
		return
	}
	b, err := json.Marshal(v)
	if err == nil {
		b = append(b, '\n')
		_, err = e.w.Write(b)
	}
	e.err = err
}

// Link writes one link. The row is kept as a string, byte for byte as hashed.
func (e *Writer) Link(l Link) error { e.line(l); return e.err }

// Close writes the trailer holding head and flushes.
func (e *Writer) Close(head Head) error {
	e.line(trailer{Head: &head})
	if e.err != nil {
		return e.err
	}
	return e.w.Flush()
}

// ExportReport is what VerifyExport found.
type ExportReport struct {
	Header Header
	Range
	// Links is how many links the file holds.
	Links int64
	// Head is the head the trailer holds; nil when the file has none.
	Head *Head
	// Notes are what the check found that is not a failure.
	Notes []string
}

// IsExport reports whether the start of a file is a chain export.
func IsExport(start []byte) bool {
	line, _, _ := bytes.Cut(start, []byte("\n"))
	var h struct {
		V int `json:"abhed_chain"`
	}
	return json.Unmarshal(line, &h) == nil && h.V > 0
}

// VerifyExport checks an export on its own, with nothing from the store: each
// link against the one before it and its own hash, and the last against the
// trailer's head, which every export ends with. heads are hashes kept elsewhere (a witness, an anchor), by
// position; each must be in the file, unchanged. The returned map holds every
// position's hash that held, for checking more heads.
func VerifyExport(r io.Reader, heads map[int64]string) (ExportReport, map[int64]string, error) {
	var rep ExportReport
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 256<<20)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return rep, nil, err
		}
		return rep, nil, errors.New("the file is empty")
	}
	if err := json.Unmarshal(sc.Bytes(), &rep.Header); err != nil || rep.Header.V == 0 {
		return rep, nil, errors.New("the file is not a chain export: its first line is no header")
	}
	if rep.Header.V > ExportVersion {
		return rep, nil, fmt.Errorf("the export is version %d; this build reads up to %d", rep.Header.V, ExportVersion)
	}
	held := map[int64]string{}
	w := NewWalker(rep.Header.From, rep.Header.Before, func(pos int64, hash string) { held[pos] = hash })
	line := 1
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if rep.Head != nil {
			w.fail(rep.Head.Pos+1, fmt.Sprintf("line %d follows the trailer: lines were added", line))
			break
		}
		var probe struct {
			Head *Head  `json:"head"`
			Pos  *int64 `json:"pos"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			w.fail(w.want, fmt.Sprintf("line %d cannot be read: %v", line, err))
			break
		}
		if probe.Head != nil && probe.Pos == nil {
			rep.Head = probe.Head
			continue
		}
		var l Link
		if err := json.Unmarshal(b, &l); err != nil {
			w.fail(w.want, fmt.Sprintf("line %d cannot be read: %v", line, err))
			break
		}
		rep.Links++
		w.Next(l)
	}
	if err := sc.Err(); err != nil {
		return rep, nil, err
	}
	walked := w.r.To
	to := int64(0)
	if rep.Head != nil {
		to = rep.Head.Pos
	}
	rep.Range = w.Done(to)
	if rep.OK() && rep.Head != nil && rep.Head.Pos > 0 && rep.Head.Hash != rep.Hash {
		rep.FirstBad = rep.Head.Pos
		rep.Reason = fmt.Sprintf("position %d is not the head the trailer holds: the file was rewritten", rep.Head.Pos)
	}
	if rep.OK() && rep.Head != nil && walked > rep.Head.Pos {
		rep.FirstBad = rep.Head.Pos + 1
		rep.Reason = "the file holds links past the trailer's head"
	}
	// Export always ends with one, so a file without it was cut.
	if rep.OK() && rep.Head == nil {
		rep.FirstBad = walked + 1
		rep.Reason = "the file has no trailer: lines were cut from its end"
	}
	for _, pos := range slices.Sorted(maps.Keys(heads)) {
		if !rep.OK() {
			break
		}
		want := heads[pos]
		got, ok := held[pos]
		switch {
		case !ok:
			rep.FirstBad = pos
			rep.Reason = fmt.Sprintf("a head kept elsewhere is at position %d, which the file does not hold: links were cut", pos)
		case got != want:
			rep.FirstBad = pos
			rep.Reason = fmt.Sprintf("position %d is not the head kept elsewhere: the chain was rewritten", pos)
		}
	}
	if rep.Header.PreChain > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%d row(s) from before the chain are not covered", rep.Header.PreChain))
	}
	return rep, held, nil
}
