package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

func todoCall(text string) model.ToolCall {
	return call("todo", map[string]any{"items": []map[string]string{{"id": "1", "text": text, "status": "pending"}}})
}

func todosIn(t *testing.T, store *MemStore, id string) []string {
	t.Helper()
	evs, _ := store.Events(id)
	var out []string
	for _, ev := range evs {
		if ev.Type == EvTodoUpdated {
			var l TodoList
			if err := json.Unmarshal(ev.Payload, &l); err != nil {
				t.Fatal(err)
			}
			for _, it := range l.Items {
				out = append(out, it.Text)
			}
		}
	}
	return out
}

// Two loops on one registry, as server sessions share one: each list is
// recorded in the record of the loop that wrote it, and nowhere else.
func TestTodoRecordsIntoTheCallingLoop(t *testing.T) {
	reg := tools.NewRegistry(TodoTool{})
	store := NewMemStore()
	for _, id := range []string{"a", "b"} {
		sess, err := tools.NewSession(tempDir(t))
		if err != nil {
			t.Fatal(err)
		}
		l := NewLoop(&scriptedAdapter{turns: []scriptedTurn{{calls: []model.ToolCall{todoCall("plan " + id)}}}},
			reg, policy.New(policy.ModeDefault), AutoApprove{}, sess, NewRecorder(store, id, ""), DefaultConfig())
		if _, err := l.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		if got := l.Todos(); len(got) != 1 || got[0].Text != "plan "+id {
			t.Fatalf("loop %s holds %v", id, got)
		}
	}
	if a, b := todosIn(t, store, "a"), todosIn(t, store, "b"); len(a) != 1 || a[0] != "plan a" || len(b) != 1 || b[0] != "plan b" {
		t.Fatalf("lists crossed records: a=%v b=%v", a, b)
	}
}

// A subagent spends from the parent's allowance as it goes, so it stops when
// the session's budget runs out rather than finishing its task past it.
func TestSubagentSpendsTheSharedBudget(t *testing.T) {
	b := NewBudget(500, 10, false)
	f := subFactory(t, []scriptedTurn{
		{calls: []model.ToolCall{call("glob", map[string]string{"pattern": "*"})}, usage: &model.Usage{InputTokens: 580, OutputTokens: 20}},
		{text: "found nothing", usage: &model.Usage{InputTokens: 400, OutputTokens: 30}},
	}, b)
	summary, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "look", Description: "look"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, string(TermMaxBudget)) || b.Spent() != 600 {
		t.Fatalf("the child ran past the budget: spent %d, %q", b.Spent(), summary)
	}
}

// Forgetting a session in memory forgets its subagents' records with it, at
// every depth, and leaves other sessions alone.
func TestMemStoreDeleteTakesTheSubagents(t *testing.T) {
	m := NewMemStore()
	for _, e := range []Event{
		{SessionID: "p", Seq: 1}, {SessionID: "c", ParentID: "p", Seq: 1},
		{SessionID: "g", ParentID: "c", Seq: 1}, {SessionID: "other", Seq: 1},
	} {
		if err := m.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.DeleteSession("p"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int{"p": 0, "c": 0, "g": 0, "other": 1} {
		if evs, _ := m.Events(id); len(evs) != want {
			t.Fatalf("%s holds %d events, want %d", id, len(evs), want)
		}
	}
}
