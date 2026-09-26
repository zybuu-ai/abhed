package eval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func denial(by string) agent.Event {
	p, _ := json.Marshal(map[string]string{"call_id": "c", "reason": "r", "by": by})
	return agent.Event{Type: agent.EvActionDenied, Payload: p}
}

// Probing the policy is refusals by the policy. Unknown tools and requests cut
// off unanswered (by: system) are not, though an unknown tool's call still
// counts as a call.
func TestPolicyProbingCountsOnlyRefusals(t *testing.T) {
	sys := []agent.Event{denial(agent.BySystem), denial(agent.BySystem), denial(agent.BySystem)}
	if hasFlag(Inspect(sys, Task{}), "policy_probing") {
		t.Fatal("denials by the system were counted as policy probing")
	}
	pol := []agent.Event{denial(agent.ByPolicy), denial(agent.ByHeadless), denial("")}
	if !hasFlag(Inspect(pol, Task{}), "policy_probing") {
		t.Fatal("three policy refusals were not flagged")
	}
}

// A run stopped part way through the corpus passes nothing it cut short and
// seeds nothing after it, even where the untouched seed would pass a check.
func TestRunStopsAtACancel(t *testing.T) {
	task := func(id string) Task {
		return Task{ID: id, Files: map[string]string{"a.txt": "x"}, Assertions: []Assertion{{Type: "file_exists", Path: "a.txt"}}}
	}
	tasks := []Task{task("t1"), task("t2"), task("t3")}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	stop := errors.New("stopped by interrupt")
	var ran []string
	runner := func(ctx context.Context, _ string, task Task) ([]agent.Event, Result, error) {
		ran = append(ran, task.ID)
		if task.ID == "t2" {
			cancel(stop)
			return nil, Result{Terminal: string(agent.TermUserInterrupt)}, nil
		}
		return nil, Result{Terminal: string(agent.TermCompleted)}, nil
	}
	root := t.TempDir()
	results, err := Run(ctx, tasks, root, runner)
	if !errors.Is(err, stop) || len(results) != 2 || !results[0].Passed || results[1].Passed || len(ran) != 2 {
		t.Fatalf("err %v, results %+v, ran %v", err, results, ran)
	}
	if _, err := os.Stat(filepath.Join(root, "t3")); !os.IsNotExist(err) {
		t.Fatal("a task after the stop was seeded")
	}
}

// A run that ended interrupted is never a pass, whatever the files say.
func TestInterruptedRunIsNotAPass(t *testing.T) {
	tasks := []Task{{ID: "t1", Files: map[string]string{"a.txt": "x"}, Assertions: []Assertion{{Type: "file_exists", Path: "a.txt"}}}}
	for _, reason := range []agent.TerminalReason{agent.TermUserInterrupt, agent.TermShutdown} {
		runner := func(context.Context, string, Task) ([]agent.Event, Result, error) {
			return nil, Result{Terminal: string(reason)}, nil
		}
		results, err := Run(context.Background(), tasks, t.TempDir(), runner)
		if err != nil || len(results) != 1 || results[0].Passed {
			t.Fatalf("%s: err %v results %+v", reason, err, results)
		}
	}
}
