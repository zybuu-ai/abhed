package model

import (
	"encoding/json"
	"testing"
)

// noticeConversation is a run's final answer followed by a background
// task's result, as the loop delivers it: an assistant task_status call and
// its tool result, never a user message.
func noticeConversation() Request {
	return Request{System: "sys", Messages: []Message{
		{Role: RoleUser, Content: "go"},
		{Role: RoleAssistant, Content: "parent done"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "bgn_01ABC", Name: "task_status", Args: json.RawMessage(`{"task_id":"01ABC"}`)}}},
		{Role: RoleTool, ToolCallID: "bgn_01ABC", Content: "result"},
	}}
}

// shapeOf round-trips a built request's messages to plain JSON values.
func shapeOf(t *testing.T, v any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func firstOf(v any) map[string]any {
	if a, ok := v.([]any); ok && len(a) > 0 {
		m, _ := a[0].(map[string]any)
		return m
	}
	return nil
}

// Each adapter sends a final answer followed by a background result as a
// well-formed request: the answer, then an assistant call to task_status,
// then its result, paired by id (by name for Gemini).
func TestAdapterAcceptsNoticeAfterFinalAnswer(t *testing.T) {
	req := noticeConversation()
	openAIShape := func(t *testing.T, msgs []map[string]any) {
		t.Helper()
		n := len(msgs)
		call := firstOf(msgs[n-2]["tool_calls"])
		fn, _ := call["function"].(map[string]any)
		if msgs[n-3]["role"] != "assistant" || msgs[n-3]["content"] != "parent done" ||
			msgs[n-2]["role"] != "assistant" || call["id"] != "bgn_01ABC" || fn["name"] != "task_status" ||
			msgs[n-1]["role"] != "tool" || msgs[n-1]["tool_call_id"] != "bgn_01ABC" {
			t.Fatalf("request: %v", msgs)
		}
	}
	t.Run("openai-compatible", func(t *testing.T) {
		openAIShape(t, shapeOf(t, NewOpenAICompatible("http://x", "k", "m", Profile{Name: "m"}).buildRequest(req).Messages))
	})
	t.Run("watsonx", func(t *testing.T) {
		openAIShape(t, shapeOf(t, NewWatsonX(WatsonXConfig{ModelID: "m", ProjectID: "p", APIKey: "k"}).buildRequest(req).Messages))
	})
	t.Run("anthropic", func(t *testing.T) {
		msgs := shapeOf(t, NewAnthropic("http://x", "k", "m", Profile{Name: "m"}).buildRequest(req).Messages)
		n := len(msgs)
		text, use, result := firstOf(msgs[n-3]["content"]), firstOf(msgs[n-2]["content"]), firstOf(msgs[n-1]["content"])
		if msgs[n-3]["role"] != "assistant" || text["text"] != "parent done" ||
			msgs[n-2]["role"] != "assistant" || use["type"] != "tool_use" || use["id"] != "bgn_01ABC" || use["name"] != "task_status" ||
			msgs[n-1]["role"] != "user" || result["type"] != "tool_result" || result["tool_use_id"] != "bgn_01ABC" {
			t.Fatalf("request: %v", msgs)
		}
	})
	t.Run("gemini", func(t *testing.T) {
		msgs := shapeOf(t, NewGemini("http://x", "k", "m", Profile{Name: "m"}).buildRequest(req).Contents)
		n := len(msgs)
		text, call, resp := firstOf(msgs[n-3]["parts"]), firstOf(msgs[n-2]["parts"]), firstOf(msgs[n-1]["parts"])
		fc, _ := call["functionCall"].(map[string]any)
		fr, _ := resp["functionResponse"].(map[string]any)
		if msgs[n-3]["role"] != "model" || text["text"] != "parent done" ||
			msgs[n-2]["role"] != "model" || fc["name"] != "task_status" ||
			msgs[n-1]["role"] != "user" || fr["name"] != "task_status" {
			t.Fatalf("request: %v", msgs)
		}
	})
}
