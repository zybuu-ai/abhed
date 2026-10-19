package local

import "github.com/zybuu-ai/abhed/internal/agent"

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
