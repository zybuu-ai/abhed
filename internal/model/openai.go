package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAICompatible speaks the OpenAI chat-completions API.
//
// This one adapter covers vLLM, SGLang, TensorRT-LLM's OpenAI server,
// llama.cpp, Ollama, and every hosted API worth supporting — which is why it
// is the primary path rather than one backend among many (docs §10).
type OpenAICompatible struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client

	// Think, when set, turns a hybrid model's thinking phase on or off for
	// every request. Nil leaves the server's default in place.
	Think *bool

	// ReasoningTags strips inline reasoning from content for models that emit
	// it in-band (e.g. <think>...</think>) rather than in a separate field.
	// Reasoning must never reach tool-argument parsing.
	ReasoningTags [2]string

	// User is sent as the request's "user" field when set. Some gateways
	// (a LiteLLM proxy with enforce_user_param) require it; a plain endpoint
	// treats it as an optional hint. Configured via the provider's extra.user.
	User string

	// Defaults are the operator's configured sampling parameters. A request
	// may override any of them; anything it leaves unset comes from here.
	Defaults Params

	// Retry bounds how long a transient failure is waited out, and Notify
	// reports one to the user.
	Retry  RetryPolicy
	Notify func(string)

	profile Profile
}

func NewOpenAICompatible(baseURL, apiKey, model string, p Profile) *OpenAICompatible {
	if p.Name == "" {
		p.Name = model
	}
	return &OpenAICompatible{
		BaseURL:       strings.TrimSuffix(baseURL, "/"),
		APIKey:        apiKey,
		Model:         model,
		HTTP:          &http.Client{Timeout: 10 * time.Minute},
		Retry:         DefaultRetry(),
		ReasoningTags: [2]string{"<think>", "</think>"},
		profile:       p,
	}
}

func (c *OpenAICompatible) Name() string     { return c.profile.Name }
func (c *OpenAICompatible) Profile() Profile { return c.profile }

// wire types

type wireMessage struct {
	Role string `json:"role"`
	// Content is a string OR an array of parts, which is what the OpenAI
	// schema allows. It stays a plain string whenever the message is text
	// only, so the body for every existing request is byte-identical to what
	// it was — the prefix cache depends on that, and 14 of the 20 providers
	// ride this adapter.
	Content    any            `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type wireRequest struct {
	Model            string        `json:"model"`
	Messages         []wireMessage `json:"messages"`
	Tools            []wireTool    `json:"tools,omitempty"`
	MaxTokens        int           `json:"max_tokens,omitempty"`
	Temperature      *float64      `json:"temperature,omitempty"`
	TopP             *float64      `json:"top_p,omitempty"`
	FrequencyPenalty *float64      `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64      `json:"presence_penalty,omitempty"`
	Seed             *int64        `json:"seed,omitempty"`
	Stop             []string      `json:"stop,omitempty"`
	Stream           bool          `json:"stream"`
	StreamOptions    *streamOpts   `json:"stream_options,omitempty"`
	ReasoningEffort  string        `json:"reasoning_effort,omitempty"`

	// User identifies the end user to the endpoint. OpenAI treats it as an
	// optional abuse-tracking hint, but a LiteLLM proxy configured with
	// enforce_user_param rejects any request without it ("You must pass a
	// 'user' json field"). Sent only when configured, so a plain endpoint
	// never sees it.
	User string `json:"user,omitempty"`

	// Sampler knobs an OpenAI-shaped API does not define, which local servers
	// (vLLM, Ollama, llama.cpp, TGI) accept and hosted ones ignore. They are
	// sent only when configured, so a hosted endpoint never sees them.
	TopK              *int     `json:"top_k,omitempty"`
	MinP              *float64 `json:"min_p,omitempty"`
	RepetitionPenalty *float64 `json:"repetition_penalty,omitempty"`

	// Think controls a hybrid-reasoning model's thinking phase.
	//
	// Separate from ReasoningEffort because the two are not the same knob and
	// not every server honours both: Ollama ignores reasoning_effort entirely
	// and reads "think", while OpenAI-style servers do the reverse. Sending
	// whichever one is configured, and omitting the other, lets one adapter
	// serve both without a per-vendor branch.
	//
	// This matters more than it looks. Measured on qwen3.8:27b through Ollama,
	// an ELI5 question took 3m22s with thinking on and 55s with it off — the
	// difference between an agent that feels interactive and one that does not.
	Think *bool `json:"think,omitempty"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireChunk struct {
	Choices []struct {
		Delta struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			Reasoning        string         `json:"reasoning"`
			ToolCalls        []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens *int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// wirePart is one element of OpenAI-style array content.
type wirePart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

// wireImageURL carries the image inline as a data: URI. Not an http URL — that
// would have the provider fetch the bytes, taking them out of the network.
type wireImageURL struct {
	URL string `json:"url"`
}

// openAIContent renders a message's content.
//
// Returns a bare string for a text-only message, which is both what the API
// prefers and what keeps existing request bodies unchanged.
func openAIContent(m Message) any {
	if len(m.Blocks) == 0 {
		return m.Content
	}
	parts := make([]wirePart, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		switch b.Kind {
		case BlockText:
			if b.Text != "" {
				parts = append(parts, wirePart{Type: "text", Text: b.Text})
			}
		case BlockImage:
			parts = append(parts, wirePart{
				Type: "image_url",
				ImageURL: &wireImageURL{
					URL: "data:" + b.MediaType + ";base64," +
						base64.StdEncoding.EncodeToString(b.Data),
				},
			})
		}
	}
	if len(parts) == 0 {
		return m.Content
	}
	return parts
}

func (c *OpenAICompatible) buildRequest(req Request) wireRequest {
	msgs := make([]wireMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		// System goes first and stays byte-identical across turns so the
		// serving layer's prefix cache can hit (docs P8).
		msgs = append(msgs, wireMessage{Role: "system", Content: req.System})
	}

	for _, m := range req.Messages {
		wm := wireMessage{Role: string(m.Role), Content: openAIContent(m)}
		switch m.Role {
		case RoleTool:
			wm.ToolCallID = m.ToolCallID
		case RoleAssistant:
			for _, tc := range m.ToolCalls {
				w := wireToolCall{ID: tc.ID, Type: "function"}
				w.Function.Name = tc.Name
				w.Function.Arguments = string(tc.Args)
				wm.ToolCalls = append(wm.ToolCalls, w)
			}
		}
		msgs = append(msgs, wm)
	}

	tools := make([]wireTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		var w wireTool
		w.Type = "function"
		w.Function.Name = t.Name
		w.Function.Description = t.Description
		w.Function.Parameters = t.InputSchema
		tools = append(tools, w)
	}

	sp := c.Defaults.Merge(req.Sampling())
	think := sp.Think
	if think == nil {
		think = c.Think // provider-level default, when the request names none
	}
	return wireRequest{
		Model:             c.Model,
		User:              c.User,
		Messages:          msgs,
		Tools:             tools,
		MaxTokens:         sp.MaxTokens,
		Temperature:       sp.Temperature,
		TopP:              sp.TopP,
		FrequencyPenalty:  sp.FrequencyPenalty,
		PresencePenalty:   sp.PresencePenalty,
		Seed:              sp.Seed,
		Stop:              sp.Stop,
		Stream:            true,
		StreamOptions:     &streamOpts{IncludeUsage: true},
		ReasoningEffort:   string(sp.Effort),
		Think:             think,
		TopK:              sp.TopK,
		MinP:              sp.MinP,
		RepetitionPenalty: sp.RepetitionPenalty,
	}
}

func (c *OpenAICompatible) Complete(ctx context.Context, req Request) (<-chan Chunk, error) {
	// Refuse an image the endpoint cannot read, rather than sending it and
	// letting the provider 400 with its own wording — or silently ignore it.
	if err := CheckVision(c.Profile(), req); err != nil {
		return nil, err
	}
	body, err := json.Marshal(c.buildRequest(req))
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	newRequest := func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if c.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
		return httpReq, nil
	}

	resp, err := send(ctx, c.HTTP, c.Retry, newRequest, c.Notify, c.fatalStatus)
	if err != nil {
		se := &StatusError{}
		if errors.As(err, &se) {
			return nil, fmt.Errorf("endpoint returned %s", se.Error())
		}
		return nil, fmt.Errorf("call %s: %w", c.BaseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	out := make(chan Chunk, 64)
	go c.stream(ctx, resp.Body, out, req.Tools)
	return out, nil
}

// offered is the tool set from the request, used only to validate a tool
// call recovered from prose — see salvage.go.
func (c *OpenAICompatible) stream(ctx context.Context, body io.ReadCloser,
	out chan<- Chunk, offered []ToolDef) {
	defer close(out)
	defer func() { _ = body.Close() }()

	// Tool calls arrive fragmented across chunks; accumulate by index.
	type pending struct {
		id   string
		name string
		args strings.Builder
	}
	calls := map[int]*pending{}
	var order []int
	// Buffered visible text, kept so a tool call the model wrote as prose can
	// be recovered when the stream ends with no structured call.
	var visible strings.Builder

	inReasoning := false
	var usage Usage
	stopReason := ""

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			out <- Chunk{Type: ChunkError, Err: ctx.Err()}
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var ch wireChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue // tolerate keepalives and partial frames
		}
		if ch.Error != nil {
			out <- Chunk{Type: ChunkError, Err: fmt.Errorf("%s: %s", ch.Error.Type, ch.Error.Message)}
			return
		}
		if ch.Usage != nil {
			usage.InputTokens = ch.Usage.PromptTokens
			usage.OutputTokens = ch.Usage.CompletionTokens
			if d := ch.Usage.PromptTokensDetails; d != nil && d.CachedTokens != nil {
				usage.CachedInputTokens, usage.CacheReported = *d.CachedTokens, true
			}
			if d := ch.Usage.CompletionTokensDetails; d != nil {
				usage.ReasoningTokens = d.ReasoningTokens
			}
		}
		if len(ch.Choices) == 0 {
			continue
		}
		choice := ch.Choices[0]
		if choice.FinishReason != "" {
			stopReason = choice.FinishReason
		}

		// Out-of-band reasoning (the field name differs across servers).
		if r := choice.Delta.ReasoningContent + choice.Delta.Reasoning; r != "" {
			out <- Chunk{Type: ChunkReasoning, Text: r}
		}

		if text := choice.Delta.Content; text != "" {
			// In-band reasoning: split on the tag boundary so reasoning never
			// reaches tool parsing or the transcript as content.
			emit, reasoning, nowInside := splitReasoning(text, inReasoning, c.ReasoningTags)
			inReasoning = nowInside
			if reasoning != "" {
				out <- Chunk{Type: ChunkReasoning, Text: reasoning}
			}
			if emit != "" {
				// Buffer alongside emitting: if the stream ends with no
				// structured tool call, this text is the only place a
				// text-encoded one can be recovered from.
				visible.WriteString(emit)
				out <- Chunk{Type: ChunkText, Text: emit}
			}
		}

		for _, tc := range choice.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			p, found := calls[idx]
			if !found {
				p = &pending{}
				calls[idx] = p
				order = append(order, idx)
			}
			if tc.ID != "" {
				p.id = tc.ID
			}
			if tc.Function.Name != "" {
				p.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				p.args.WriteString(tc.Function.Arguments)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		out <- Chunk{Type: ChunkError, Err: fmt.Errorf("read stream: %w", err)}
		return
	}

	// A model that wrote its tool call as prose leaves the loop with nothing
	// to dispatch, and the session ends silently after one turn. Recover the
	// call rather than stalling; see salvage.go for why this is conservative.
	if len(order) == 0 {
		if tc, found := salvageToolCall(visible.String(), offered); found {
			out <- Chunk{Type: ChunkToolCall, ToolCall: &tc}
			out <- Chunk{Type: ChunkDone, StopReason: "tool_use", Usage: &usage}
			return
		}
	}

	for _, idx := range order {
		p := calls[idx]
		if p.name == "" {
			continue
		}
		args := strings.TrimSpace(p.args.String())
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			// Surface malformed arguments as an error chunk rather than
			// passing garbage to a tool; the loop turns this into a message
			// the model can correct.
			out <- Chunk{Type: ChunkError, Err: fmt.Errorf(
				"model produced invalid JSON arguments for %s: %s", p.name, truncate(args, 200))}
			continue
		}
		id := p.id
		if id == "" {
			id = fmt.Sprintf("call_%d", idx)
		}
		out <- Chunk{Type: ChunkToolCall, ToolCall: &ToolCall{
			ID: id, Name: p.name, Args: json.RawMessage(args),
		}}
	}

	out <- Chunk{Type: ChunkDone, Usage: &usage, StopReason: stopReason}
}

// splitReasoning separates in-band reasoning from visible content, tracking
// whether the stream is currently inside a reasoning block across chunk
// boundaries.
func splitReasoning(text string, inside bool, tags [2]string) (content, reasoning string, nowInside bool) {
	open, close := tags[0], tags[1]
	if open == "" || close == "" {
		return text, "", inside
	}
	var c, r strings.Builder
	for text != "" {
		if inside {
			if i := strings.Index(text, close); i >= 0 {
				r.WriteString(text[:i])
				text = text[i+len(close):]
				inside = false
				continue
			}
			r.WriteString(text)
			break
		}
		if i := strings.Index(text, open); i >= 0 {
			c.WriteString(text[:i])
			text = text[i+len(open):]
			inside = true
			continue
		}
		c.WriteString(text)
		break
	}
	return c.String(), r.String(), inside
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// CountTokens estimates prompt size. This is a heuristic: exact counts require
// the model's tokenizer, which varies by family. Compaction thresholds are set
// with margin so an estimate is sufficient (docs §07).
func (c *OpenAICompatible) CountTokens(req Request) (int, error) {
	return estimateTokens(req), nil
}

// fatalStatus: this adapter has no status it can recognise as hopeless, so
// every retryable failure is retried.
func (c *OpenAICompatible) fatalStatus(*StatusError) bool { return false }
