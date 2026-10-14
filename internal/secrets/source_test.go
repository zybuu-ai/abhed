package secrets

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type memSource struct {
	mu   sync.Mutex
	m    map[string]string
	err  error
	read int
}

func (s *memSource) Load(context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.read++
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]string{}
	for k, v := range s.m {
		out[k] = v
	}
	return out, nil
}

func (s *memSource) Set(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.m[name] = value
	return nil
}

func (s *memSource) Remove(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
	return nil
}

func (s *memSource) fail(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// A store over a source reads, hands out and redacts its values as a file
// store does, and is written by name.
func TestSourceStore(t *testing.T) {
	src := &memSource{m: map[string]string{"API_KEY": "fake-source-value-1"}}
	s := OpenSource("vault team-a", src)
	if v, err := s.Value("API_KEY"); err != nil || v != "fake-source-value-1" {
		t.Fatalf("value %q %v", v, err)
	}
	if err := s.Set("OTHER", "fake-source-value-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("lower", "fake-source-value-3"); err == nil {
		t.Fatal("a bad name reached the source")
	}
	if names, _ := s.Names(); strings.Join(names, ",") != "API_KEY,OTHER" {
		t.Fatalf("names %v", names)
	}
	fresh := s.Session()
	if got := string(fresh.Redact([]byte(`{"a":"fake-source-value-2"}`))); got != `{"a":"[secret:OTHER]"}` {
		t.Fatalf("redacted %s", got)
	}
	// A value added later is redacted from then on; one removed stays redacted.
	_ = s.Set("LATER", "fake-source-value-4")
	_ = s.Remove("OTHER")
	if got := string(fresh.Redact([]byte(`{"a":"fake-source-value-4 fake-source-value-2"}`))); got != `{"a":"[secret:LATER] [secret:OTHER]"}` {
		t.Fatalf("redacted after a change %s", got)
	}
	if err := s.Delete(); err == nil {
		t.Fatal("a driver's store was deleted whole")
	}
}

// A source that cannot be read hands out nothing, refuses a session, and
// withholds every payload.
func TestSourceFailsClosed(t *testing.T) {
	src := &memSource{m: map[string]string{"API_KEY": "fake-source-value-1"}}
	s := OpenSource("vault team-a", src)
	fresh := s.Session()
	src.fail(errors.New("connection refused"))
	if _, err := s.Value("API_KEY"); err == nil || !strings.Contains(err.Error(), "vault team-a") {
		t.Fatalf("a value from an unreachable source: %v", err)
	}
	if _, err := s.LoadRedactor(); err == nil || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("a session over an unreachable source: %v", err)
	}
	if out := fresh.Redact([]byte(`{"a":"nothing secret"}`)); string(out) == `{"a":"nothing secret"}` {
		t.Fatal("a payload passed while the source was unreachable")
	}
	if err := s.Set("NEW", "fake-source-value-9"); err == nil {
		t.Fatal("a write to an unreachable source succeeded")
	}
	src.fail(nil)
	if got := string(fresh.Redact([]byte(`{"a":"fake-source-value-1"}`))); got != `{"a":"[secret:API_KEY]"}` {
		t.Fatalf("after the source came back: %s", got)
	}

	// A key that is not a secret name refuses the whole store.
	bad := OpenSource("k8s team-a", &memSource{m: map[string]string{"api.key": "fake-source-value-1"}})
	if _, err := bad.Names(); err == nil {
		t.Fatal("a stray key was accepted")
	}
}
