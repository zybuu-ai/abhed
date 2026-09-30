package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// deniedSteps are the steps recorded on action.denied, in order.
func deniedSteps(t *testing.T, store *MemStore) []string {
	t.Helper()
	evs, _ := store.Events("sess1")
	var out []string
	for _, e := range evs {
		if e.Type == EvActionDenied {
			var p map[string]string
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			out = append(out, p["step"])
		}
	}
	return out
}

// lastToolResult is what the model was told about the last call.
func lastToolResult(l *Loop) string {
	msgs := l.Adapter.(*scriptedAdapter).gotRequests
	last := msgs[len(msgs)-1].Messages
	for i := len(last) - 1; i >= 0; i-- {
		if last[i].Role == model.RoleTool {
			return last[i].Content
		}
	}
	return ""
}

// An edit or write the tool would refuse for want of a read is refused
// before anyone is asked, and the model is told to read first; one that can
// succeed is still put to the person.
func TestReadBeforeEditIsCheckedBeforeAsking(t *testing.T) {
	for _, c := range []struct {
		name  string
		calls func(dir string) [][]model.ToolCall
		setup func(t *testing.T, dir string)
		asked int
		told  string
	}{
		{"unread edit", func(dir string) [][]model.ToolCall {
			return [][]model.ToolCall{{call("edit", map[string]string{"path": filepath.Join(dir, "a.txt"), "old_string": "x", "new_string": "y"})}}
		}, nil, 0, "has not been read this session"},
		{"unread overwrite", func(dir string) [][]model.ToolCall {
			return [][]model.ToolCall{{call("write", map[string]string{"path": filepath.Join(dir, "a.txt"), "content": "y"})}}
		}, nil, 0, "has not been read this session"},
		{"edit of a missing file", func(dir string) [][]model.ToolCall {
			return [][]model.ToolCall{{call("edit", map[string]string{"path": filepath.Join(dir, "gone.txt"), "old_string": "x", "new_string": "y"})}}
		}, nil, 0, "File not found"},
		{"changed since read", func(dir string) [][]model.ToolCall {
			return [][]model.ToolCall{
				{call("read", map[string]string{"path": filepath.Join(dir, "a.txt")})},
				{call("edit", map[string]string{"path": filepath.Join(dir, "a.txt"), "old_string": "x", "new_string": "y"})},
			}
		}, nil, 0, "changed on disk"},
		{"read in an earlier turn", func(dir string) [][]model.ToolCall {
			return [][]model.ToolCall{
				{call("read", map[string]string{"path": filepath.Join(dir, "a.txt")})},
				{call("edit", map[string]string{"path": filepath.Join(dir, "a.txt"), "old_string": "x", "new_string": "y"})},
			}
		}, nil, 1, ""},
		{"read in the same turn", func(dir string) [][]model.ToolCall {
			return [][]model.ToolCall{{
				call("read", map[string]string{"path": filepath.Join(dir, "a.txt")}),
				call("edit", map[string]string{"path": filepath.Join(dir, "a.txt"), "old_string": "x", "new_string": "y"}),
			}}
		}, nil, 1, ""},
		{"a new file", func(dir string) [][]model.ToolCall {
			return [][]model.ToolCall{{call("write", map[string]string{"path": filepath.Join(dir, "new.txt"), "content": "y"})}}
		}, nil, 1, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := tempDir(t)
			if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var turns []scriptedTurn
			for _, cs := range c.calls(dir) {
				turns = append(turns, scriptedTurn{calls: cs})
			}
			turns = append(turns, scriptedTurn{text: "ok"})
			l, store := harnessIn(t, dir, turns, policy.ModeDefault, true)
			counter := &askCounter{}
			l.Approver = counter
			if c.name == "changed since read" {
				// The file changes between the read and the edit.
				l.Tools.Add(touchAfterRead{path: filepath.Join(dir, "a.txt")})
			}
			if _, err := l.Run(context.Background(), "change a"); err != nil {
				t.Fatal(err)
			}
			if counter.asked != c.asked {
				t.Fatalf("asked %d times, want %d (denied at %v)", counter.asked, c.asked, deniedSteps(t, store))
			}
			if c.told != "" {
				if steps := deniedSteps(t, store); len(steps) != 1 || steps[0] != "precheck" {
					t.Fatalf("denied at %v", steps)
				}
				if got := lastToolResult(l); !strings.Contains(got, c.told) {
					t.Fatalf("the model was told %q", got)
				}
			}
		})
	}
}

// touchAfterRead replaces read for one test: it reads, then the file changes.
type touchAfterRead struct{ path string }

func (touchAfterRead) Name() string            { return "read" }
func (touchAfterRead) Description() string     { return "read" }
func (touchAfterRead) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (touchAfterRead) Mutates() bool           { return false }
func (r touchAfterRead) Run(_ context.Context, s *tools.Session, _ json.RawMessage) tools.Result {
	s.MarkRead(r.path, "x\n")
	_ = os.WriteFile(r.path, []byte("changed\n"), 0o644)
	return tools.Result{Content: "x"}
}
