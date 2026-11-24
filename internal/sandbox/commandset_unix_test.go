//go:build unix

package sandbox

import (
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Adding a command to a sandbox's set never reads another command's state
// while that command's own Wait writes it (-race reports it): the status
// line runs commands of one sandbox side by side. Only this test's own
// short-lived children are started; none is signalled.
func TestAddingACommandDoesNotRaceAnotherCommandsWait(t *testing.T) {
	var c commandSet
	for range 20 {
		running := exec.Command("/bin/sh", "-c", "exit 0")
		c.add(running)
		if err := running.Start(); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = running.Wait()
		}()
		for range 50 {
			c.add(exec.Command("/bin/true"))
		}
		wg.Wait()
		forgetCommand(running)
	}
}

// A command that was never followed, once nothing else holds it, leaves the
// set, so a sandbox running many short commands does not keep them all.
func TestTheSetForgetsCommandsNothingHolds(t *testing.T) {
	var c commandSet
	for range 100 {
		c.add(exec.Command("/bin/true"))
	}
	kept := exec.Command("/bin/true")
	c.add(kept)
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		c.add(kept) // prunes what was collected
		c.mu.Lock()
		n := len(c.cmds)
		c.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the set still holds %d commands", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	runtime.KeepAlive(kept)
}
