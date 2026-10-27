package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
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

// InfillResult is the text to insert and what it cost. Dropped says why an
// answer was not used as code, such as prose from a chat model.
type InfillResult struct {
	Text         string
	InputTokens  int
	OutputTokens int
	Dropped      string
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
	res := InfillResult{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}
	res.Text, res.Dropped = CleanInfill(out.Choices[0].Text, req.Prefix, req.Suffix, true)
	return res, nil
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
// used, cleaned of reasoning, fences, prose and repeated code.
func InfillByChat(ctx context.Context, a Adapter, req InfillRequest) (InfillResult, error) {
	sys := "You are a code completion engine, not an assistant. Output only the exact code to insert at " + cursor +
		". No explanation, no prose, no markdown, no code fences, and do not repeat the code before or after " +
		cursor + ". If nothing fits, output nothing."
	user := "Language: " + orElse(req.Language, "unknown") + "\n" + req.Prefix + cursor + req.Suffix
	r := Request{System: sys, Messages: []Message{{Role: RoleUser, Content: user}},
		// Room for a reasoning model to think briefly; the answer itself is cut short.
		MaxTokens: max(4*orInt(req.MaxTokens, 128), 512)}
	sampling := a.Profile().Sampling
	if sampling.Temperature {
		t := 0.1
		r.Temperature = &t
	}
	if sampling.Stop && len(req.Stop) > 0 {
		r.Stop = req.Stop
	}
	ch, err := a.Complete(ctx, r)
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
	res.Text, res.Dropped = CleanInfill(b.String(), req.Prefix, req.Suffix, false)
	return res, nil
}

// Reasoning written into the text by models that do not send it apart.
var (
	thinkBlock    = regexp.MustCompile(`(?s)<think(?:ing)?>.*?(?:</think(?:ing)?>|\z)`)
	harmonyFinal  = regexp.MustCompile(`(?s)^.*<\|channel\|>final<\|message\|>`)
	harmonyTokens = regexp.MustCompile(`<\|[a-z_]+\|>`)
	analysisFinal = regexp.MustCompile(`(?s)^\s*analysis.*?assistantfinal`)
	firstFence    = regexp.MustCompile("(?s)```[^\n`]*\n(.*?)(?:\n?```|\\z)")
	proseOpener   = regexp.MustCompile(`(?i)^(?:here(?:'s| is| are)|below|sure|certainly|of course|okay|i(?:'ll|'m| will| would| can| have)|let me|the following|this (?:code|function|implementation|is)|to (?:complete|implement)|you can|great)\b`)
)

// prose reports an answer that reads as an explanation, not code. From a
// fill-in-the-middle endpoint it takes both an opener and markdown, since a
// comment being completed may start like a sentence.
func prose(text string, fim bool) bool {
	opener, markdown := proseOpener.MatchString(text), strings.Contains(text, "**")
	if fim {
		return opener && markdown
	}
	return opener || markdown
}

// maxInfillLines bounds a suggestion; an inline completion is a few lines.
const maxInfillLines = 40

// CleanInfill makes a model's answer into the text to insert at the cursor:
// reasoning and fences stripped, prose dropped (with why), and code repeated
// from either side of the cursor removed. fim is an answer from a
// fill-in-the-middle endpoint, which is taken as code unless it reads as prose.
func CleanInfill(text, prefix, suffix string, fim bool) (string, string) {
	if m := harmonyFinal.FindStringIndex(text); m != nil {
		text = text[m[1]:]
	}
	text = analysisFinal.ReplaceAllString(text, "")
	text = thinkBlock.ReplaceAllString(text, "")
	text = harmonyTokens.ReplaceAllString(text, "")
	if !fim {
		text = strings.TrimLeft(text, "\n")
		text = strings.TrimRight(text, " \t\n")
	}
	if m := firstFence.FindStringSubmatch(text); m != nil {
		text = m[1]
	} else if trimmed := strings.TrimSpace(text); prose(trimmed, fim) {
		return "", "the model answered in prose, not code"
	}
	// Code the cursor's own line already has, repeated at the start.
	if i := strings.LastIndexByte(prefix, '\n'); !fim {
		line := strings.TrimLeft(prefix[i+1:], " \t")
		if line != "" && strings.HasPrefix(strings.TrimLeft(text, " \t"), line) {
			text = strings.TrimLeft(text, " \t")[len(line):]
		}
	}
	// And the suffix's first line, repeated at the end, when it is more than
	// a brace a nested block might close with too.
	if head, _, _ := strings.Cut(strings.TrimLeft(suffix, "\n"), "\n"); len(strings.TrimSpace(head)) >= 4 &&
		strings.HasSuffix(strings.TrimRight(text, "\n"), head) {
		text = strings.TrimSuffix(strings.TrimRight(text, "\n"), head)
	}
	if lines := strings.SplitAfter(text, "\n"); len(lines) > maxInfillLines {
		text = strings.Join(lines[:maxInfillLines], "")
	}
	if !fim {
		text = strings.TrimRight(text, "\n")
	}
	if strings.TrimSpace(text) == "" {
		return "", ""
	}
	return text, ""
}
