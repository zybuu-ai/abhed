package local

import (
	"fmt"
	"io"
)

// ExportFormat is the version of the export trailer. Version 2 carries
// whether the record verified when it was exported.
const ExportFormat = 2

// ExportOptions shape an export.
type ExportOptions struct {
	// Unverified exports a record that fails verification, marked so in
	// its trailer; without it such a record is refused.
	Unverified bool
}

// Export writes session id as it is recorded, line for line, then a trailer
// holding the head the record stored, not one worked out from the lines, so
// the copy verifies elsewhere only as far as the record did here. The lines
// are already redacted: they are what the record holds.
func (s *Store) Export(id string, w io.Writer, o ExportOptions) (Head, error) {
	if err := checkID("session", id); err != nil {
		return Head{}, err
	}
	rep, err := s.Verify(id)
	if err != nil {
		return Head{}, err
	}
	if !rep.OK && !o.Unverified {
		return Head{}, &UnverifiedError{Report: rep}
	}
	s.mu.Lock()
	h := s.held[id]
	s.mu.Unlock()
	if h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
	}
	data, err := readOwn(s.Path(id))
	if err != nil {
		return Head{}, fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	for _, raw := range scan(data).raws {
		if _, err := w.Write(append(raw, '\n')); err != nil {
			return Head{}, err
		}
	}
	head, _ := s.readHead(id)
	tr := exportTrailer{Head: head, Session: id, Format: ExportFormat, Verified: boolPtr(rep.OK)}
	if !rep.OK {
		tr.Unverified = fmt.Sprintf("at seq %d, %s", rep.FirstBad, rep.Reason)
	}
	t, err := encode(tr)
	if err != nil {
		return head, err
	}
	_, err = w.Write(append(t, '\n'))
	return head, err
}

func boolPtr(b bool) *bool { return &b }
