package remote

import (
	"testing"
	"time"
)

// A fork drops the hosts connected after its step and keeps the earlier ones.
func TestSessionHostsForgetAfter(t *testing.T) {
	s := &sessionHosts{hosts: map[string]*Host{}}
	s.add(&Host{cfg: HostConfig{Name: "old"}})
	mid := time.Now()
	time.Sleep(2 * time.Millisecond)
	s.add(&Host{cfg: HostConfig{Name: "new"}})
	s.ForgetAfter(mid)
	if _, ok := s.get("old"); !ok {
		t.Fatal("an earlier host was dropped")
	}
	if _, ok := s.get("new"); ok {
		t.Fatal("a later host was kept")
	}
}
