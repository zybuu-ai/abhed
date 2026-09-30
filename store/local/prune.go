package local

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Pruned is what a prune removed.
type Pruned struct {
	ID string
	// Head is the removed session's last line, kept in its tombstone.
	Head Head
	// Blobs is how many checkpoint contents went with it.
	Blobs int
}

// Prune removes session id and the subagent sessions it started: a
// tombstone with each one's head goes into the index first, then the files
// and the checkpoints nothing else names. It is refused while another
// process holds the session. This is the only way anything leaves the
// record, and it is never automatic unless the managed configuration sets
// record.retention_days.
func (s *Store) Prune(id, by, reason string) ([]Pruned, error) {
	e, ok := s.index.get(id)
	if !ok || e.Pruned {
		return nil, fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	ids := []string{id}
	all, err := s.index.entries()
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(ids); i++ {
		for _, c := range all {
			if c.Subagent && c.Parent == ids[i] && !c.Pruned {
				ids = append(ids, c.ID)
			}
		}
	}
	for _, sid := range ids {
		if s.Held(sid) {
			return nil, fmt.Errorf("session %s is open in this process; close it first", sid)
		}
	}
	// Every one is taken first, so a session another process holds stops
	// the prune before anything is removed.
	var taken []*session
	for _, sid := range ids {
		h, err := s.acquire(sid, false, nil)
		if err != nil {
			for _, t := range taken {
				_ = s.Release(t.id)
			}
			return nil, err
		}
		taken = append(taken, h)
	}
	gone := map[string]bool{}
	var refs []string
	var out []Pruned
	for _, h := range taken {
		h.mu.Lock()
		_ = h.sync(s)
		head := h.last
		refs = append(refs, agent.BlobRefs(h.events)...)
		h.mu.Unlock()
		if err := s.index.append(indexLine{Op: opPrune, ID: h.id, By: by, Reason: reason,
			HeadLines: head.Lines, HeadSeq: head.Seq, HeadHash: head.Hash}); err != nil {
			return out, err
		}
		_ = s.Release(h.id)
		for _, p := range []string{s.Path(h.id), s.headPath(h.id), s.lockPath(h.id)} {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return out, err
			}
		}
		gone[h.id] = true
		out = append(out, Pruned{ID: h.id, Head: head})
	}
	n, err := s.collect(refs, gone)
	if len(out) > 0 {
		out[0].Blobs = n
	}
	return out, err
}

// collect removes the blobs in refs that no remaining session names.
func (s *Store) collect(refs []string, gone map[string]bool) (int, error) {
	if len(refs) == 0 {
		return 0, nil
	}
	kept := map[string]bool{}
	all, err := s.index.entries()
	if err != nil {
		return 0, err
	}
	for _, e := range all {
		if e.Pruned || gone[e.ID] {
			continue
		}
		evs, err := s.Events(e.ID)
		if err != nil {
			// A record that cannot be read may name any blob: keep them all.
			return 0, nil //nolint:nilerr // keeping blobs is the safe answer
		}
		for _, r := range agent.BlobRefs(evs) {
			kept[r] = true
		}
	}
	n := 0
	for _, r := range refs {
		if kept[r] {
			continue
		}
		kept[r] = true // once
		if err := s.blobs.remove(r); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// PruneOlder prunes the top-level sessions last updated before cutoff. A
// session another process holds is skipped and named in skipped.
func (s *Store) PruneOlder(cutoff time.Time, by, reason string) (pruned []Pruned, skipped []string, err error) {
	entries, err := s.index.list(Filter{All: true})
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if !e.Updated.Before(cutoff) || s.Held(e.ID) {
			continue
		}
		p, err := s.Prune(e.ID, by, reason)
		if errors.Is(err, ErrHeldElsewhere) {
			skipped = append(skipped, e.ID)
			continue
		}
		if err != nil {
			return pruned, skipped, err
		}
		pruned = append(pruned, p...)
	}
	return pruned, skipped, nil
}
