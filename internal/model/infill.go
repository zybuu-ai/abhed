package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Fill in the middle: text to insert between a prefix and a suffix, for an
// editor's inline completion. No tools, no conversation; text only.

// InfillRequest is the code around the cursor and the bounds on the answer.
type InfillRequest struct {
	Prefix    string
	Suffix    string
	Language  string
	MaxTokens int
	Stop      []string
}

// InfillResult is the text to insert and what it cost.
type InfillResult struct {
	Text         string
	InputTokens  int
	OutputTokens int
}

// Infiller is an adapter that speaks a fill-in-the-middle endpoint itself.
type Infiller interface {
	Infill(ctx context.Context, req InfillRequest) (InfillResult, error)
}

// maxInfillAnswer bounds what an endpoint's answer may be.
const maxInfillAnswer = 1 << 20

// Infill asks the OpenAI-compatible completions endpoint, with a suffix:
// what Ollama, vLLM, llama.cpp and others serve for code models.
func (c *OpenAICompatible) Infill(ctx context.Context, req InfillRequest) (InfillResult, error) {
	body := map[string]any{"model": c.Model, "prompt": req.Prefix, "suffix": req.Suffix,
		"max_tokens": orInt(req.MaxTokens, 128), "temperature": 0.2, "stream": false}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return InfillResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/completions", bytes.NewReader(raw))
	if err != nil {
		return InfillResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return InfillResult{}, fmt.Errorf("call %s: %w", c.BaseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxInfillAnswer))
	if err != nil {
		return InfillResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return InfillResult{}, fmt.Errorf("endpoint returned %w", &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data[:min(len(data), 4096)])), Attempts: 1})
	}
	var out struct {
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return InfillResult{}, fmt.Errorf("the endpoint's answer is not a completion: %w", err)
	}
	if len(out.Choices) == 0 {
		return InfillResult{}, errors.New("the endpoint answered no completion")
	}
	return InfillResult{Text: out.Choices[0].Text, InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}, nil
}

func orInt(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}

// cursor marks where the answer goes, in a chat model's prompt.
const cursor = "<|abhed:cursor|>"

// InfillByChat asks any adapter for the text at the cursor through a chat
// turn, for a provider with no fill-in-the-middle endpoint. The code is
// given as untrusted text in the user's turn, and only the reply's text is
// used.
func InfillByChat(ctx context.Context, a Adapter, req InfillRequest) (InfillResult, error) {
	sys := "You complete code at a cursor. Reply with only the text to insert at " + cursor +
		": no explanation, no code fences, nothing before or after it. Reply with nothing if nothing fits."
	user := "Language: " + orElse(req.Language, "unknown") + "\n" + req.Prefix + cursor + req.Suffix
	ch, err := a.Complete(ctx, Request{System: sys, Messages: []Message{{Role: RoleUser, Content: user}},
		MaxTokens: orInt(req.MaxTokens, 128)})
	if err != nil {
		return InfillResult{}, err
	}
	var b strings.Builder
	var res InfillResult
	for c := range ch {
		if c.Err != nil {
			return InfillResult{}, c.Err
		}
		if c.Type == ChunkText {
			b.WriteString(c.Text)
		}
		if c.Usage != nil {
			res.InputTokens, res.OutputTokens = c.Usage.InputTokens, c.Usage.OutputTokens
		}
		if b.Len() > maxInfillAnswer {
			break
		}
	}
	text := strings.TrimSpace(b.String())
	// A chat model may fence its answer though told not to.
	if strings.HasPrefix(text, "```") {
		if i := strings.Index(text, "\n"); i >= 0 {
			text = text[i+1:]
		}
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
	}
	res.Text = strings.TrimRight(text, "\n")
	return res, nil
}
