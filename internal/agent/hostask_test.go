package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// decisionSeen approves and keeps the decision each ask was put with.
type decisionSeen struct{ got []policy.Result }

func (d *decisionSeen) Approve(_ context.Context, _ string, _ json.RawMessage, r policy.Result) (bool, error) {
	d.got = append(d.got, r)
	return true, nil
}

// hostAskLoop is a loop whose bash runs nothing and says, for each line, why
// host git would need the person.
func hostAskLoop(t *testing.T, line string, ask func(callID, cwd, command string) (string, string, error), allow, askRule string) (*Loop, *MemStore, *decisionSeen) {
	t.Helper()
	dir := tempDir(t)
	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	bash := tools.Bash{Sandbox: func(ctx context.Context, _, _ string) *exec.Cmd { return exec.CommandContext(ctx, "true") }, HostAsk: ask}
	pol := policy.New(policy.ModeDefault)
	if allow != "" {
		if err := pol.AddAllow(allow); err != nil {
			t.Fatal(err)
		}
	}
	if askRule != "" {
		if err := pol.AddAsk(askRule); err != nil {
			t.Fatal(err)
		}
	}
	seen := &decisionSeen{}
	l := NewLoop(&scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": line})}},
		{text: "ok"},
	}}, tools.NewRegistry(bash), pol, seen, sess, NewRecorder(store, "sess1", ""), DefaultConfig())
	return l, store, seen
}

// A brokered line policy would allow is asked about under host git's step,
// with its reason, and the call's id reaches the broker.
func TestHostAskTurnsAnAllowIntoAnAsk(t *testing.T) {
	var gotID string
	l, _, seen := hostAskLoop(t, "git fetch", func(callID, _, _ string) (string, string, error) {
		gotID = callID
		return "git fetch reaches git.example although the sandbox's network is off", "hostgit", nil
	}, "bash(git *)", "")
	l.Run(context.Background(), "fetch")
	if len(seen.got) != 1 || seen.got[0].Step != "hostgit" || !strings.Contains(seen.got[0].Reason, "network is off") {
		t.Fatalf("asked %+v", seen.got)
	}
	if gotID != "cbash" {
		t.Fatalf("the broker judged call %q", gotID)
	}
}

// An ask rule's ask keeps its own step, so no grant for host git answers
// it, and gains the reason.
func TestHostAskKeepsAnAskRulesStep(t *testing.T) {
	l, _, seen := hostAskLoop(t, "git push", func(string, string, string) (string, string, error) {
		return "git push runs through host git as you", "hostgit", nil
	}, "", "bash(git push*)")
	l.Run(context.Background(), "push")
	if len(seen.got) != 1 || seen.got[0].Step != "ask" || !strings.Contains(seen.got[0].Reason, "runs through host git") {
		t.Fatalf("asked %+v", seen.got)
	}
}

// A line the broker refuses is refused before anyone is asked.
func TestHostAskRefusalIsNotPutToAPerson(t *testing.T) {
	l, store, seen := hostAskLoop(t, "git -c core.sshCommand=x fetch", func(string, string, string) (string, string, error) {
		return "", "", errors.New("an option before the command could change what host git runs")
	}, "bash(git *)", "")
	l.Run(context.Background(), "fetch")
	if len(seen.got) != 0 {
		t.Fatalf("asked %+v", seen.got)
	}
	evs, _ := store.Events("sess1")
	denied := payloads[map[string]string](evs, EvActionDenied)
	if len(denied) != 1 || denied[0]["step"] != "precheck" || !strings.Contains(denied[0]["reason"], "before the command") {
		t.Fatalf("denied: %+v", denied)
	}
}

// With nothing to ask, policy decides alone.
func TestHostAskSilentLeavesPolicy(t *testing.T) {
	l, _, seen := hostAskLoop(t, "git fetch", func(string, string, string) (string, string, error) { return "", "", nil }, "bash(git *)", "")
	l.Run(context.Background(), "fetch")
	if len(seen.got) != 0 {
		t.Fatalf("asked %+v", seen.got)
	}
}
