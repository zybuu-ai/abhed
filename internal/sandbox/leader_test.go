package sandbox

import (
	"syscall"
	"testing"
)

// fakeSession replaces the sweep's view of the system with a script: the
// members each listing returns, in turn, and a log of the signals sent.
type fakeSession struct {
	listings [][]int
	sent     map[int][]syscall.Signal
}

func (f *fakeSession) install(t *testing.T) {
	t.Helper()
	f.sent = map[int][]syscall.Signal{}
	saved := [...]any{listMembers, signalOne, startOf, canSweep}
	listMembers = func(int) []int {
		if len(f.listings) == 0 {
			return nil
		}
		m := f.listings[0]
		if len(f.listings) > 1 {
			f.listings = f.listings[1:]
		}
		return m
	}
	signalOne = func(pid, _ int, sig syscall.Signal) bool {
		f.sent[pid] = append(f.sent[pid], sig)
		return true
	}
	startOf = func(int) (uint64, bool) { return 7, true }
	canSweep = func() (bool, string) { return true, "" }
	t.Cleanup(func() {
		listMembers = saved[0].(func(int) []int)
		signalOne = saved[1].(func(int, int, syscall.Signal) bool)
		startOf = saved[2].(func(int) (uint64, bool))
		canSweep = saved[3].(func() (bool, string))
	})
}

var named = Leader{Pid: 100, start: 7, ok: true}

// A process that appears after the kill pass, as one continued by the kernel
// when a stopped member exits can, is killed by the re-check that follows.
func TestSweepKillsWhatAppearsAfterTheKill(t *testing.T) {
	f := &fakeSession{listings: [][]int{
		{101}, {101}, // stop passes: 101, then nothing new
		{102}, // after the kill pass, a new member
		{},    // and then none
	}}
	f.install(t)
	if why := named.sweep(); why != "" {
		t.Fatalf("sweep: %s", why)
	}
	if got := f.sent[102]; len(got) == 0 || got[len(got)-1] != killSignal {
		t.Fatalf("the member that appeared after the kill pass was not killed: %v", f.sent)
	}
}

// A session still growing after every pass is reported, so it is logged.
func TestSweepSaysWhenMembersRemain(t *testing.T) {
	f := &fakeSession{listings: [][]int{{103}}}
	f.install(t)
	if named.sweep() == "" {
		t.Fatal("members left after every pass went unreported")
	}
}
