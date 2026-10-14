package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// stepTool is a pipeline step's tool: entered is signalled as it starts, and
// it returns once release is closed.
type stepTool struct {
	name     string
	mutates  bool
	entered  chan struct{}
	release  chan struct{}
	finished *atomic.Bool
}

func (s stepTool) Name() string            { return s.name }
func (s stepTool) Description() string     { return "a step" }
func (s stepTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s stepTool) Mutates() bool           { return s.mutates }
func (s stepTool) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	if s.entered != nil {
		close(s.entered)
	}
	if s.release != nil {
		<-s.release
	}
	if s.finished != nil {
		s.finished.Store(true)
	}
	return tools.Result{Content: "ok"}
}

// A mutating pipeline step runs apart from every other pipeline step on its
// loop: another pipeline's read-only step waits until it is done.
func TestMutatingPipelineStepRunsAlone(t *testing.T) {
	sess, err := tools.NewSession(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var readDone atomic.Bool
	reg := tools.NewRegistry(
		stepTool{name: "change", mutates: true, entered: entered, release: release},
		stepTool{name: "look", finished: &readDone},
	)
	l := NewLoop(&scriptedAdapter{}, reg, policy.New(policy.ModeBypass), AutoApprove{Yes: true}, sess,
		NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	first, second := &Steps{loop: l, via: "skill a pipeline"}, &Steps{loop: l, via: "skill b pipeline"}

	changed := make(chan error, 1)
	go func() {
		_, err := first.Run(context.Background(), "change", json.RawMessage(`{}`), 0)
		changed <- err
	}()
	<-entered
	looked := make(chan error, 1)
	go func() {
		_, err := second.Run(context.Background(), "look", json.RawMessage(`{}`), 0)
		looked <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if readDone.Load() {
		t.Fatal("another pipeline's step ran while a mutating step was running")
	}
	close(release)
	for _, ch := range []chan error{changed, looked} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a step did not finish")
		}
	}
	if !readDone.Load() {
		t.Fatal("the read-only step never ran")
	}
}
