package local

import (
	"fmt"
	"io"
	"os"
)

// ExportFormat is the version of the export trailer.
const ExportFormat = 1

// Export writes session id as it is recorded, line for line, then a trailer
// holding its head, so the copy can be verified with VerifyFile on another
// machine. The lines are already redacted: they are what the record holds.
func (s *Store) Export(id string, w io.Writer) (Head, error) {
	if err := checkID("session", id); err != nil {
		return Head{}, err
	}
	s.mu.Lock()
	h := s.held[id]
	s.mu.Unlock()
	if h != nil {
		h.mu.Lock()
		_ = h.sync(s)
		defer h.mu.Unlock()
	}
	data, err := os.ReadFile(s.Path(id))
	if err != nil {
		return Head{}, fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	sc := scan(data)
	var head Head
	for _, raw := range sc.raws {
		if _, err := w.Write(append(raw, '\n')); err != nil {
			return head, err
		}
		head.Lines++
		if l, err := parseLine(raw); err == nil {
			head.Seq, head.Hash = l.Seq, l.Hash
		} else {
			head.Hash = hashBytes(raw)
		}
	}
	t, err := encode(exportTrailer{Head: head, Session: id, Format: ExportFormat})
	if err != nil {
		return head, err
	}
	_, err = w.Write(append(t, '\n'))
	return head, err
}
