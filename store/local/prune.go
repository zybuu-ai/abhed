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
	// Head is the removed session's head as the record stored it, or as
	// the index last recorded it when the file or its head was gone.
	Head Head
	// Blobs is how many checkpoint contents went with it.
	Blobs int
	// Missing is set when the file was already gone, and Verified when the
	// record verified as it was pruned.
	Missing  bool
	Verified bool
}

// Prune removes session id and the subagent sessions it started. For each,
// a tombstone goes into the index first: its head as stored, whether its
// file was already missing, and whether it verified. Then the files go, and
// the checkpoints nothing else names. It is refused while any of them is
// open in a process, this one included. Nothing is created or repaired on
// the way: a record already damaged or gone is pruned as found, and says
// so. This is the only way anything leaves the record, and it is never
// automatic unless the managed configuration sets record.retention_days.
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
	byID := map[string]Entry{}
	for _, c := range all {
		byID[c.ID] = c
	}
	for i := 0; i < len(ids); i++ {
		for _, c := range all {
			if c.Subagent && c.Parent == ids[i] && !c.Pruned {
				ids = append(ids, c.ID)
			}
		}
	}
	// Every one is locked first, so a session another process holds stops
	// the prune before anything is removed. Only the lock is taken: the
	// record itself is not opened for writing.
	locks := map[string]*os.File{}
	release := func() {
		for _, lk := range locks {
			_ = unlock(lk)
			_ = lk.Close()
		}
	}
	for _, sid := range ids {
		if s.Held(sid) {
			release()
			return nil, fmt.Errorf("session %s is open in this process; close it first", sid)
		}
		lk, err := os.OpenFile(s.lockPath(sid), os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- an id checked to be a plain name
		if err != nil {
			release()
			return nil, err
		}
		if got, err := tryLock(lk); err != nil || !got {
			_ = lk.Close()
			release()
			return nil, fmt.Errorf("session %s: %w", sid, ErrHeldElsewhere)
		}
		locks[sid] = lk
	}
	defer release()
	gone := map[string]bool{}
	var refs []string
	keepAll := false
	var out []Pruned
	for _, sid := range ids {
		p := Pruned{ID: sid}
		if _, err := os.Lstat(s.Path(sid)); errors.Is(err, os.ErrNotExist) {
			p.Missing = true
		}
		rep, verr := s.Verify(sid)
		p.Verified = verr == nil && rep.OK
		if head, ok := s.readHead(sid); ok {
			p.Head = head
		} else {
			p.Head = byID[sid].Head // the index's last recorded end
		}
		if evs, _, err := s.readEvents(sid); err == nil {
			refs = append(refs, agent.BlobRefs(evs)...)
		} else {
			keepAll = true // what it named cannot be read: keep every blob
		}
		tomb := indexLine{Op: opPrune, ID: sid, By: by, Reason: reason,
			HeadLines: p.Head.Lines, HeadSeq: p.Head.Seq, HeadHash: p.Head.Hash,
			Missing: p.Missing, Verified: boolPtr(p.Verified)}
		if !p.Verified {
			tomb.Unverified = rep.Reason
			if verr != nil {
				tomb.Unverified = verr.Error()
			}
		}
		if err := s.index.append(tomb); err != nil {
			return out, err
		}
		for _, f := range []string{s.Path(sid), s.headPath(sid), s.lockPath(sid)} {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				return out, err
			}
		}
		gone[sid] = true
		out = append(out, p)
	}
	if keepAll {
		return out, nil
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
