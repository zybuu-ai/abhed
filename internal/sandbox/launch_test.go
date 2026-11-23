package sandbox

import (
	"errors"
	"os/exec"
	"testing"
)

// What waits on a command's end hears it once: from Exited, or as an error
// when the command is released unstarted.
func TestOnExitHearsTheEndOnce(t *testing.T) {
	var got []error
	ran := exec.Command("true")
	OnExit(ran, func(err error) { got = append(got, err) })
	Exited(ran, nil)
	Exited(ran, errors.New("again"))
	if len(got) != 1 || got[0] != nil {
		t.Fatalf("heard %v", got)
	}
	got = nil
	unstarted := exec.Command("true")
	OnExit(unstarted, func(err error) { got = append(got, err) })
	Release(unstarted)
	Exited(unstarted, nil)
	if len(got) != 1 || !errors.Is(got[0], errNotStarted) {
		t.Fatalf("heard %v for a command released unstarted", got)
	}
}
