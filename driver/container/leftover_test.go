package container

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/driver"
)

// leftoverEngine answers as an engine holding a container under the
// sandbox's name, labelled as given, until it is removed.
func leftoverEngine(labels string, running bool) (*fakeRunner, func() bool) {
	f := &fakeRunner{podman: true}
	var mu sync.Mutex
	present, removed := true, false
	state := "false"
	if running {
		state = "true"
	}
	f.respond = func(args []string) ([]byte, error, bool) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case args[0] == "container" && slices.Contains(args, "inspect") && slices.Contains(args, "abhed-sbx-sbx_0123456789abcdef"):
			if !present {
				return nil, &engineError{args: args, stderr: "no such container"}, true
			}
			if slices.ContainsFunc(args, func(a string) bool { return a == `{{.State.Status}} {{.State.ExitCode}} {{.State.OOMKilled}}` }) {
				return nil, nil, false
			}
			return []byte(labels + " " + state + "\n"), nil, true
		case args[0] == "rm" && slices.Contains(args, "abhed-sbx-sbx_0123456789abcdef"):
			if present {
				removed = true
			}
			present = false
			return nil, nil, true
		case args[0] == "run" && present:
			return nil, &engineError{args: args, stderr: `the container name "abhed-sbx-sbx_0123456789abcdef" is already in use`}, true
		case args[0] == "run":
			present = true
		}
		return nil, nil, false
	}
	return f, func() bool { mu.Lock(); defer mu.Unlock(); return removed }
}

// A stopped container this driver left for the id, as a restarted server
// finds one, is removed and the sandbox made; another's is never touched.
func TestCreateClearsItsOwnStoppedLeftover(t *testing.T) {
	f, removed := leftoverEngine("sbx_0123456789abcdef podman", false)
	d, ws := testDriver(t, f, Options{}, nil)
	if _, err := d.Create(context.Background(), spec(ws)); err != nil {
		t.Fatalf("create over a stopped leftover of its own: %v", err)
	}
	if !removed() {
		t.Fatal("the leftover was not removed")
	}

	for name, labels := range map[string]string{
		"another driver's": "sbx_0123456789abcdef docker",
		"another id's":     "sbx_ffffffffffffffff podman",
		"unlabelled":       "<no value> <no value>",
	} {
		f, removed := leftoverEngine(labels, false)
		d, ws := testDriver(t, f, Options{}, nil)
		if _, err := d.Create(context.Background(), spec(ws)); !errors.Is(err, driver.ErrExists) {
			t.Errorf("%s: %v, want exists", name, err)
		}
		if removed() {
			t.Errorf("%s: a container not this driver's for the id was removed", name)
		}
	}

	// One of its own that still runs is not removed before the create.
	f, _ = leftoverEngine("sbx_0123456789abcdef podman", true)
	d, ws = testDriver(t, f, Options{}, nil)
	if _, err := d.Create(context.Background(), spec(ws)); !errors.Is(err, driver.ErrExists) {
		t.Errorf("over a running leftover: %v, want exists", err)
	}
}
