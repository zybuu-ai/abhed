package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// WatsonX adapts IBM watsonx.ai to Abhed's Adapter interface.
//
// It is close to the OpenAI shape but differs in three ways that each matter:
//
//   - Authentication is an IAM bearer token minted from an API key, not the
//     key itself. Tokens last an hour, so they are cached and refreshed
//     before expiry rather than exchanged per request.
//   - The model is named in the body as model_id, alongside a project_id or
//     space_id that scopes it. Sending both is rejected.
//   - Reasoning arrives as reasoning_content. gpt-oss-120b reasons on almost
//     every turn, so this is the normal path, not an edge case.
type WatsonX struct {
	BaseURL   string
	APIKey    string
	ProjectID string
	SpaceID   string
	ModelID   string
	Version   string
	// IAMURL is overridable for CPD, which mints tokens itself.
	IAMURL string

	// Defaults are the operator's configured sampling parameters, overridden
	// per request by anything the request itself sets.
	Defaults Params

	profile Profile
	client  *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewWatsonX builds a watsonx adapter.
func NewWatsonX(cfg WatsonXConfig) *WatsonX {
	if cfg.Version == "" {
		// A date-versioned API: this is the version the chat endpoint has
		// been stable on.
		cfg.Version = "2024-05-01"
	}
	if cfg.IAMURL == "" {
		cfg.IAMURL = "https://iam.cloud.ibm.com/identity/token"
	}
	if cfg.Profile.Name == "" {
		cfg.Profile.Name = cfg.ModelID
	}
	cfg.Profile.SupportsTools = true
	cfg.Profile.SupportsStream = true
	return &WatsonX{
		BaseURL:   strings.TrimSuffix(cfg.BaseURL, "/"),
		APIKey:    cfg.APIKey,
		ProjectID: cfg.ProjectID,
		SpaceID:   cfg.SpaceID,
		ModelID:   cfg.ModelID,
		Version:   cfg.Version,
		IAMURL:    cfg.IAMURL,
		profile:   cfg.Profile,
		Defaults:  cfg.Defaults,
		client:    &http.Client{Timeout: 10 * time.Minute},
	}
}

// WatsonXConfig configures the adapter.
type WatsonXConfig struct {
	BaseURL   string
	APIKey    string
	ProjectID string
	SpaceID   string
	ModelID   string
	Version   string
	IAMURL    string
	Profile   Profile
	// Defaults are the operator's configured sampling parameters, overridden
	// per request by anything the request itself sets.
	Defaults Params
}

func (w *WatsonX) Name() string     { return "watsonx:" + w.ModelID }
func (w *WatsonX) Profile() Profile { return w.profile }

// bearer returns a valid IAM token, exchanging the API key when needed.
func (w *WatsonX) bearer(ctx context.Context) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Refresh a minute early: a token that expires mid-request produces a 401
	// that reads like a credentials problem.
	if w.token != "" && time.Now().Before(w.expires.Add(-time.Minute)) {
		return w.token, nil
	}

	form := url.Values{
		"grant_type": {"urn:ibm:params:oauth:grant-type:apikey"},
		"apikey":     {w.APIKey},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.IAMURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach IBM IAM: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Message     string `json:"errorMessage"`
	}
	_ = json.Unmarshal(body, &out) // a non-JSON body leaves the token empty, which is handled just below
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		msg := out.Message
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		return "", fmt.Errorf("watsonx authentication failed: %s "+
			"(check the API key, and that it belongs to the account owning the "+
			"project or space)", msg)
	}
	w.token = out.AccessToken
	w.expires = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return w.token, nil
}

// ---------------------------------------------------------------- wire types

type wxRequest struct {
	ModelID     string      `json:"model_id"`
	ProjectID   string      `json:"project_id,omitempty"`
	SpaceID     string      `json:"space_id,omitempty"`
	Messages    []wxMessage `json:"messages"`
	Tools       []wxTool    `json:"tools,omitempty"`
	MaxTokens   int         `json:"max_tokens,omitempty"`
	Temperature *float64    `json:"temperature,omitempty"`
	TopP        *float64    `json:"top_p,omitempty"`
	Stop        []string    `json:"stop,omitempty"`
}

type wxMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content,omitempty"`
	ToolCalls  []wxToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type wxTool struct {
	Type     string     `json:"type"`
	Function wxFunction `json:"function"`
}

type wxFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wxToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wxChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          string       `json:"content"`
			ReasoningContent string       `json:"reasoning_content"`
			ToolCalls        []wxToolCall `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content          string       `json:"content"`
			ReasoningContent string       `json:"reasoning_content"`
			ToolCalls        []wxToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Errors []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

func (w *WatsonX) buildRequest(req Request) wxRequest {
	sp := w.Defaults.Merge(req.Sampling())
	out := wxRequest{
		ModelID:     w.ModelID,
		ProjectID:   w.ProjectID,
		SpaceID:     w.SpaceID,
		MaxTokens:   sp.MaxTokens,
		Temperature: sp.Temperature,
		TopP:        sp.TopP,
		Stop:        sp.Stop,
	}
	// The system prompt is a separate field on Request, not a message. Missing
	// it entirely is silent: the model still answers, just without any of the
	// working method, tool guidance or environment it was supposed to have —
	// which showed up as the agent doing one tool call and stopping.
	if req.System != "" {
		out.Messages = append(out.Messages, wxMessage{Role: "system", Content: req.System})
	}
	// Tool results reference their call by id. When a call was salvaged the id
	// was synthesised above, so the result has to be given the same one or
	// watsonx rejects the pairing.
	synthesised := map[string]string{} // original (possibly empty) -> synthesised
	for _, m := range req.Messages {
		wm := wxMessage{Role: string(m.Role), ToolCallID: m.ToolCallID}
		// wxMessage.Content is already `any`, so OpenAI-style array content
		// serialises without a type change here.
		if c := openAIContent(m); c != "" {
			wm.Content = c
		}
		for i, tc := range m.ToolCalls {
			var call wxToolCall
			call.ID = tc.ID
			if call.ID == "" {
				// A salvaged call has no id — the model never emitted one.
				// watsonx rejects the request without it, so synthesise a
				// stable one rather than failing the turn.
				call.ID = fmt.Sprintf("call_%d_%d", len(out.Messages), i)
			}
			call.Type = "function"
			call.Function.Name = tc.Name
			call.Function.Arguments = string(tc.Args)
			if tc.ID == "" {
				synthesised[tc.Name] = call.ID
			}
			wm.ToolCalls = append(wm.ToolCalls, call)
		}
		if wm.Role == "tool" && wm.ToolCallID == "" {
			// Pair with the most recent synthesised id.
			for _, id := range synthesised {
				wm.ToolCallID = id
			}
		}
		out.Messages = append(out.Messages, wm)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, wxTool{
			Type: "function",
			Function: wxFunction{
				Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
			},
		})
	}
	return out
}

// Complete streams a response.
func (w *WatsonX) Complete(ctx context.Context, req Request) (<-chan Chunk, error) {
	token, err := w.bearer(ctx)
	if err != nil {
		return nil, err
	}
	// Refuse an image the endpoint cannot read, rather than sending it and
	// letting the provider 400 with its own wording — or silently ignore it.
	if err := CheckVision(w.Profile(), req); err != nil {
		return nil, err
	}
	body, err := json.Marshal(w.buildRequest(req))
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/ml/v1/text/chat_stream?version=%s", w.BaseURL, w.Version)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := w.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("watsonx request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("watsonx returned %s: %s",
			resp.Status, wxErrorMessage(raw))
	}

	out := make(chan Chunk, 64)
	go w.stream(resp.Body, out, req.Tools)
	return out, nil
}

func (w *WatsonX) stream(body io.ReadCloser, out chan<- Chunk, offered []ToolDef) {
	defer close(out)
	defer func() { _ = body.Close() }()

	type pending struct {
		id   string
		name string
		args strings.Builder
	}
	calls := map[int]*pending{}
	var order []int
	var visible strings.Builder
	// Reasoning is normally discarded, but gpt-oss-120b sometimes ends its
	// reasoning with the tool ARGUMENTS and never emits a tool_calls delta —
	// leaving the loop nothing to dispatch and the turn silently empty. Keep
	// it so that case can be recovered.
	var reasoningBuf strings.Builder
	var usage Usage
	stopReason := ""

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var ch wxChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue // tolerate keepalives and partial frames
		}
		if len(ch.Errors) > 0 {
			out <- Chunk{Type: ChunkError,
				Err: fmt.Errorf("%s: %s", ch.Errors[0].Code, ch.Errors[0].Message)}
			return
		}
		if ch.Usage != nil {
			usage.InputTokens = ch.Usage.PromptTokens
			usage.OutputTokens = ch.Usage.CompletionTokens
		}
		if len(ch.Choices) == 0 {
			continue
		}
		choice := ch.Choices[0]
		if choice.FinishReason != "" {
			stopReason = choice.FinishReason
		}

		// The streaming endpoint uses delta; a non-streaming fallback would
		// use message. Accept either so one code path covers both.
		content := choice.Delta.Content
		reasoning := choice.Delta.ReasoningContent
		toolCalls := choice.Delta.ToolCalls
		if content == "" && reasoning == "" && len(toolCalls) == 0 {
			content = choice.Message.Content
			reasoning = choice.Message.ReasoningContent
			toolCalls = choice.Message.ToolCalls
		}

		if reasoning != "" {
			reasoningBuf.WriteString(reasoning)
			out <- Chunk{Type: ChunkReasoning, Text: reasoning}
		}
		if content != "" {
			visible.WriteString(content)
			out <- Chunk{Type: ChunkText, Text: content}
		}
		for _, tc := range toolCalls {
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
		stoppedDone(out, usage)
		return
	}

	// Same salvage as the OpenAI adapter: a model that wrote its call as prose
	// leaves the loop with nothing to dispatch (see salvage.go). Reasoning is
	// searched too, because that is where gpt-oss-120b puts it: the reasoning
	// ends "let's do deeper search.{\"pattern\":\"**/*.go\"}" with no
	// tool_calls delta at all.
	if len(order) == 0 {
		for _, text := range []string{visible.String(), reasoningBuf.String()} {
			if tc, found := salvageToolCall(text, offered); found {
				out <- Chunk{Type: ChunkToolCall, ToolCall: &tc}
				out <- Chunk{Type: ChunkDone, StopReason: "tool_use", Usage: &usage}
				return
			}
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
			out <- Chunk{Type: ChunkError, Err: fmt.Errorf(
				"tool call %s had malformed arguments: %s", p.name, args)}
			continue
		}
		out <- Chunk{Type: ChunkToolCall, ToolCall: &ToolCall{
			ID: p.id, Name: p.name, Args: json.RawMessage(args)}}
	}
	out <- Chunk{Type: ChunkDone, StopReason: stopReason, Usage: &usage}
}

// CountTokens estimates prompt size. watsonx exposes a tokenization endpoint,
// but calling it per turn would add a network round trip to every budget
// check; the same heuristic the OpenAI adapter uses is close enough for
// compaction decisions.
func (w *WatsonX) CountTokens(req Request) (int, error) {
	total := 0
	for _, m := range req.Messages {
		total += len(m.Content)/4 + 4
		for _, tc := range m.ToolCalls {
			total += len(tc.Args)/4 + len(tc.Name)/4 + 8
		}
	}
	for _, t := range req.Tools {
		total += (len(t.Description) + len(t.InputSchema)) / 4
	}
	return total, nil
}

// wxErrorMessage pulls the readable part out of a watsonx error body.
func wxErrorMessage(raw []byte) string {
	var body struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		if len(body.Errors) > 0 {
			return body.Errors[0].Code + ": " + body.Errors[0].Message
		}
		if body.Message != "" {
			return body.Message
		}
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}
