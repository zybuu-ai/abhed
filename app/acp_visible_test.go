package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	abhed "github.com/zybuu-ai/abhed/sdk"
)

// A path that redraws itself must not reach the editor's card as raw text:
// the title shows the escapes and says the call carries hidden characters.
func TestACPTitleShowsHiddenCharacters(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"path": "/ws/a.txt\u200d\r\u001b[2K/ws/readme.md", "content": "hi"})
	newACPAgent = func(_ context.Context, opts abhed.Options) (acpAgent, error) {
		return &scriptedACPAgent{opts: opts, args: args}, nil
	}
	defer func() {
		newACPAgent = func(ctx context.Context, o abhed.Options) (acpAgent, error) { return abhed.New(ctx, o) }
	}()
	titles := make(chan string, 4)
	cl := newACPClient(t, func(_ string, params json.RawMessage) any {
		var p struct {
			ToolCall struct {
				Title string `json:"title"`
			} `json:"toolCall"`
		}
		_ = json.Unmarshal(params, &p)
		titles <- p.ToolCall.Title
		return chosen(params, "reject_once")
	})
	cl.request(1, "initialize", map[string]any{"protocolVersion": 1})
	created := cl.request(2, "session/new", map[string]any{"cwd": "/ws"})
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(created.Result, &sess)
	_, updates := cl.prompt(3, map[string]any{"sessionId": sess.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "x"}}})
	got := []string{<-titles}
	for _, u := range updates {
		if u["sessionUpdate"] == "tool_call" {
			title, _ := u["title"].(string)
			got = append(got, title)
		}
	}
	if len(got) < 2 {
		t.Fatalf("expected the card and the ask, got %q", got)
	}
	for _, title := range got {
		if strings.ContainsAny(title, "\r\u001b\u200d") || !strings.Contains(title, "⟨U+200D⟩") || !strings.Contains(title, `\r\x1b[2K`) {
			t.Errorf("title not made visible: %q", title)
		}
	}
	if !strings.Contains(got[0], "hidden or control characters") {
		t.Errorf("the permission title does not warn: %q", got[0])
	}
}

// permissionTitle runs one scripted write with args and returns the title of
// the permission request the editor is sent.
func permissionTitle(t *testing.T, args json.RawMessage) string {
	t.Helper()
	newACPAgent = func(_ context.Context, opts abhed.Options) (acpAgent, error) {
		return &scriptedACPAgent{opts: opts, args: args}, nil
	}
	defer func() {
		newACPAgent = func(ctx context.Context, o abhed.Options) (acpAgent, error) { return abhed.New(ctx, o) }
	}()
	titles := make(chan string, 4)
	cl := newACPClient(t, func(_ string, params json.RawMessage) any {
		var p struct {
			ToolCall struct {
				Title string `json:"title"`
			} `json:"toolCall"`
		}
		_ = json.Unmarshal(params, &p)
		titles <- p.ToolCall.Title
		return chosen(params, "reject_once")
	})
	cl.request(1, "initialize", map[string]any{"protocolVersion": 1})
	created := cl.request(2, "session/new", map[string]any{"cwd": "/ws"})
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(created.Result, &sess)
	cl.prompt(3, map[string]any{"sessionId": sess.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "x"}}})
	return <-titles
}

// The note covers every argument, not only the ones the title draws.
func TestACPNoteCoversEveryArgument(t *testing.T) {
	long := strings.Repeat("line\n", 19) + "x\u202ey\n" + strings.Repeat("line\n", 5)
	cases := map[string]map[string]string{
		"content past the preview": {"path": "/ws/a.txt", "content": long},
		"command carriage return":  {"path": "/ws/a.txt", "command": "ls\rrm -rf x"},
		"manifest escape":          {"path": "/ws/a.txt", "manifest": `{"kind":"ConfigMap","data":{"k":"\u001b[2J"}}`},
		"prompt joiner":            {"path": "/ws/a.txt", "prompt": "a\u200db"},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			args, _ := json.Marshal(a)
			if got := permissionTitle(t, args); !strings.Contains(got, "hidden or control characters") {
				t.Errorf("no note: %q", got)
			}
		})
	}
	if got := permissionTitle(t, json.RawMessage(`{"path":"/ws/a.txt","content":"a\tb\n"}`)); strings.Contains(got, "hidden") {
		t.Errorf("a plain call is noted: %q", got)
	}
}
