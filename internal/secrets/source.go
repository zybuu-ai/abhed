package secrets

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Source keeps a store's values somewhere other than a file, such as a
// credential driver (Vault, Kubernetes Secrets). It is read whole and written
// by name. A source that cannot be reached fails: the store then refuses to
// hand out a value and withholds every payload it would redact.
type Source interface {
	Load(ctx context.Context) (map[string]string, error)
	Set(ctx context.Context, name, value string) error
	Remove(ctx context.Context, name string) error
}

// SourceTimeout bounds each call a store makes of its source.
var SourceTimeout = 15 * time.Second

// OpenSource is a store whose values src keeps; name says which store it is
// in messages and never holds a value.
func OpenSource(name string, src Source) *Store {
	return &Store{name: name, src: src, mu: new(sync.Mutex)}
}

func (s *Store) srcCall(fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), SourceTimeout)
	defer cancel()
	if err := fn(ctx); err != nil {
		return fmt.Errorf("%s: %w", s.name, err)
	}
	return nil
}

// loadSource reads every value, refusing a name a secret cannot have so a
// driver's stray key never becomes something a command is handed.
func (s *Store) loadSource() (map[string]string, error) {
	var m map[string]string
	err := s.srcCall(func(ctx context.Context) error {
		var err error
		m, err = s.src.Load(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if !ValidName(k) {
			return nil, fmt.Errorf("%s holds %q, which is not a secret name", s.name, k)
		}
		out[k] = v
	}
	return out, nil
}

// sourceStamp stands for the values a source holds now, so a Fresh redactor
// rebuilds only when they change; a source that cannot be read is not ok.
func (s *Store) sourceStamp() freshStamp {
	s.mu.Lock()
	m, err := s.load()
	s.mu.Unlock()
	if err != nil {
		return freshStamp{}
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, k := range names {
		fmt.Fprintf(h, "%d:%s%d:%s", len(k), k, len(m[k]), m[k])
	}
	st := freshStamp{ok: true}
	copy(st.digest[:], h.Sum(nil))
	return st
}

// hint says how to repair a store that cannot be loaded.
func (s *Store) hint() string {
	if s.src != nil {
		return "Check that " + s.name + " can be reached and that this server may read it"
	}
	return fixHint
}
