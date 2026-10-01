package agent

import (
	"context"
	"encoding/json"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// TodoTool is the todo tool bound to whichever loop calls it. The list is
// recorded as todo.updated in that loop's record, so one registry can be
// shared by many sessions, and a subagent's list stays in its own record.
type TodoTool struct{ tools.Todo }

func (t TodoTool) Run(ctx context.Context, sess *tools.Session, raw json.RawMessage) tools.Result {
	inner := t.Todo
	if p, _ := ctx.Value(parentKey{}).(*parentLink); p != nil && p.loop != nil {
		l, own := p.loop, inner.OnUpdate
		inner.OnUpdate = func(items []tools.TodoItem, note string) {
			l.RecordTodos(TodosOf(items), note)
			if own != nil {
				own(items, note)
			}
		}
	}
	return inner.Run(ctx, sess, raw)
}

// TodosOf converts the tool's items to the event payload's.
func TodosOf(items []tools.TodoItem) []Todo {
	out := make([]Todo, 0, len(items))
	for _, i := range items {
		out = append(out, Todo{ID: i.ID, Text: i.Text, Status: i.Status})
	}
	return out
}
