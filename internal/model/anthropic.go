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

// Anthropic speaks the Messages API.
//
// This is not the OpenAI adapter with a different URL. The shapes genuinely
// differ: the system prompt is a top-level field rather than a message, content
// is a list of typed blocks rather than a string, tool calls arrive as
// tool_use blocks whose arguments stream as JSON fragments, and the streaming
// protocol is a set of named events rather than one delta shape.
//
// Two of those differences carry real weight for Abhed. Caching is explicit —
// a cache_control marker on the last system block is what makes the stable
// prefix cheap, which is exactly the economics docs P8 is about, and it has to
// be asked for rather than inferred. And thinking is a first-class request
// field with its own response blocks, so reasoning arrives structured instead
// of having to be scraped out of the text with tag matching.
type Anthropic struct {
	BaseURL string
	APIKey  string
	Model   string
	Version string // anthropic-version header
	HTTP    *http.Client

	// Beta carries anthropic-beta feature flags, for capabilities that are
	// gated behind one.
	Beta []string

	// Bearer authenticates with an OAuth token rather than an API key.
	//
	// A Claude Pro or Max subscription is not an API key: it is an OAuth
	// credential, sent as Authorization: Bearer, and the two headers are not
	// interchangeable. Supporting it means a developer who already pays for a
	// subscription can drive Abhed with it instead of buying API credit
	// separately, which for an evaluation is often the difference between
	// trying the thing and not.
	//
	// Abhed does not run the browser flow that mints these tokens: that is
	// Anthropic's, it changes, and reimplementing someone else's login is a
	// standing liability. `claude setup-token` prints a long-lived one, and
	// this reads it.
	Bearer string

	// Defaults are the operator's configured sampling parameters, overridden
	// per request by anything the request itself sets.
	Defaults Params

	// Retry bounds how long a transient failure is waited out. A rate limit
	// ending a session discards everything it had established, which is a far
	// worse outcome than a few seconds of silence.
	Retry RetryPolicy
	// Notify reports a retry to the user, since a silent pause in an
	// interactive session is indistinguishable from a hang.
	Notify func(string)

	profile Profile
}

const defaultAnthropicVersion = "2023-06-01"

func NewAnthropic(baseURL, apiKey, model string, p Profile) *Anthropic {
	if p.Name == "" {
		p.Name = model
	}
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	return &Anthropic{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Version: defaultAnthropicVersion,
		HTTP:    &http.Client{Timeout: 10 * time.Minute},
		Retry:   DefaultRetry(),
		profile: p,
	}
}

func (c *Anthropic) Name() string     { return c.profile.Name }
func (c *Anthropic) Profile() Profile { return c.profile }

// ---------------------------------------------------------------- wire types

type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	// thinking
	Thinking string `json:"thinking,omitempty"`

	// Image content. Anthropic takes base64 with an explicit media type.
	Source *anthropicSource `json:"source,omitempty"`

	CacheControl *anthropicCache `json:"cache_control,omitempty"`
}

type anthropicCache struct {
	Type string `json:"type"`
}

// anthropicSource carries an image. Base64 rather than a URL: a URL would make
// the provider fetch it, which sends the bytes out of the network on a
// deployment whose premise is that they stay in.
type anthropicSource struct {
	Type      string `json:"type"`       // always "base64"
	MediaType string `json:"media_type"` // image/png, image/jpeg, ...
	Data      string `json:"data"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicThinking struct {
	Type string `json:"type"`
	// BudgetTokens applies to the older enabled-with-budget form. Adaptive
	// thinking omits it; sending it to a model that removed it is a 400.
	BudgetTokens *int `json:"budget_tokens,omitempty"`
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	System        []anthropicBlock   `json:"system,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream"`
	Thinking      *anthropicThinking `json:"thinking,omitempty"`
}

// anthropicBlocks renders one message's content.
//
// The common case is still a single text block, so a text-only message
// produces exactly the body it did before — which matters, because an
// unchanged prefix is what keeps the cache warm.
func anthropicBlocks(m Message) []anthropicBlock {
	if len(m.Blocks) == 0 {
		return []anthropicBlock{{Type: "text", Text: m.Content}}
	}
	out := make([]anthropicBlock, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		switch b.Kind {
		case BlockText:
			if b.Text != "" {
				out = append(out, anthropicBlock{Type: "text", Text: b.Text})
			}
		case BlockImage:
			out = append(out, anthropicBlock{
				Type: "image",
				Source: &anthropicSource{
					Type:      "base64",
					MediaType: b.MediaType,
					Data:      base64.StdEncoding.EncodeToString(b.Data),
				},
			})
		}
	}
	if len(out) == 0 {
		out = append(out, anthropicBlock{Type: "text", Text: m.Content})
	}
	return out
}

func (c *Anthropic) buildRequest(req Request) anthropicRequest {
	sp := c.Defaults.Merge(req.Sampling())

	var msgs []anthropicMessage
	for _, m := range req.Messages {
		switch m.Role {
		case RoleTool:
			// A tool result is a user-role message carrying a tool_result
			// block, not a role of its own.
			msgs = append(msgs, anthropicMessage{
				Role: "user",
				Content: []anthropicBlock{{
					Type: "tool_result", ToolUseID: m.ToolCallID,
					Content: m.Content, IsError: m.IsError,
				}},
			})
		case RoleAssistant:
			var blocks []anthropicBlock
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				args := tc.Args
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				blocks = append(blocks, anthropicBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: args,
				})
			}
			if len(blocks) == 0 {
				// An assistant turn with neither text nor calls is not a legal
				// message; drop it rather than have the API reject the whole
				// request for an empty block list.
				continue
			}
			msgs = append(msgs, anthropicMessage{Role: "assistant", Content: blocks})
		default:
			msgs = append(msgs, anthropicMessage{
				Role:    "user",
				Content: anthropicBlocks(m),
			})
		}
	}

	var system []anthropicBlock
	if req.System != "" {
		// The marker on the last system block is what makes the stable prefix
		// — prompt plus tools — a cache read on every later turn. Without it
		// the whole prefix is re-billed each request, which is the difference
		// docs P8 measures.
		system = []anthropicBlock{{
			Type: "text", Text: req.System,
			CacheControl: &anthropicCache{Type: "ephemeral"},
		}}
	}

	var tools []anthropicTool
	for _, t := range req.Tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, anthropicTool{
			Name: t.Name, Description: t.Description, InputSchema: schema,
		})
	}

	maxTokens := sp.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.profile.MaxOutputTokens
	}
	if maxTokens <= 0 {
		maxTokens = 8192 // the API requires this field
	}

	out := anthropicRequest{
		Model:         c.Model,
		Messages:      msgs,
		System:        system,
		Tools:         tools,
		MaxTokens:     maxTokens,
		Temperature:   sp.Temperature,
		TopP:          sp.TopP,
		TopK:          sp.TopK,
		StopSequences: sp.Stop,
		Stream:        true,
	}

	// Thinking is requested explicitly. A budget is only sent when one was
	// configured: current models removed budget_tokens and reject it, while
	// adaptive thinking takes no budget at all.
	switch {
	case sp.ThinkingBudget != nil:
		out.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: sp.ThinkingBudget}
	case sp.Think != nil && *sp.Think, sp.Effort != EffortNone:
		out.Thinking = &anthropicThinking{Type: "adaptive"}
	}
	return out
}

// ---------------------------------------------------------------- streaming

type anthropicEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	Message *struct {
		StopReason string          `json:"stop_reason"`
		Usage      *anthropicUsage `json:"usage"`
	} `json:"message"`

	ContentBlock *anthropicBlock `json:"content_block"`

	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`

	Usage *anthropicUsage `json:"usage"`

	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (c *Anthropic) Complete(ctx context.Context, req Request) (<-chan Chunk, error) {
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
			c.BaseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("anthropic-version", orElse(c.Version, defaultAnthropicVersion))
		// A subscription token and an API key are different credentials on
		// different headers. Sending both would let the server pick, which makes
		// "which account paid for this" depend on someone else's precedence rules.
		switch {
		case c.Bearer != "":
			httpReq.Header.Set("Authorization", "Bearer "+c.Bearer)
			// The OAuth beta is what makes a subscription token acceptable on the
			// Messages API; without it the request is rejected as unauthenticated.
			httpReq.Header.Set("anthropic-beta", betaHeader(c.Beta, "oauth-2025-04-20"))
		case c.APIKey != "":
			httpReq.Header.Set("x-api-key", c.APIKey)
			if len(c.Beta) > 0 {
				httpReq.Header.Set("anthropic-beta", strings.Join(c.Beta, ","))
			}
		}

		return httpReq, nil
	}

	resp, err := send(ctx, c.HTTP, c.Retry, newRequest, c.Notify, c.fatalStatus)
	if err != nil {
		se := &StatusError{}
		if errors.As(err, &se) {
			if c.Bearer != "" && looksLikeSubscriptionRefusal(se) {
				return nil, errSubscriptionRestricted
			}
			return nil, fmt.Errorf("anthropic returned %s", se.Error())
		}
		return nil, fmt.Errorf("%s is unreachable: %w", c.BaseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("anthropic returned %s: %s",
			resp.Status, strings.TrimSpace(string(msg)))
	}

	out := make(chan Chunk, 32)
	go c.stream(ctx, resp, out)
	return out, nil
}

func (c *Anthropic) stream(ctx context.Context, resp *http.Response, out chan<- Chunk) {
	defer close(out)
	defer resp.Body.Close()

	// Tool arguments arrive as JSON fragments across many events, keyed by the
	// block index they belong to. They are only parseable once the block ends.
	type pending struct {
		id, name string
		args     strings.Builder
	}
	blocks := map[int]*pending{}
	usage := Usage{}
	stop := ""

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}

		var ev anthropicEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue // a malformed frame is not worth ending the turn over
		}

		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				usage.InputTokens += ev.Message.Usage.InputTokens
				usage.CachedInputTokens += ev.Message.Usage.CacheReadInputTokens
			}

		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				blocks[ev.Index] = &pending{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			}

		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					out <- Chunk{Type: ChunkText, Text: ev.Delta.Text}
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					out <- Chunk{Type: ChunkReasoning, Text: ev.Delta.Thinking}
				}
			case "input_json_delta":
				if b := blocks[ev.Index]; b != nil {
					b.args.WriteString(ev.Delta.PartialJSON)
				}
			}

		case "content_block_stop":
			b := blocks[ev.Index]
			if b == nil {
				continue
			}
			delete(blocks, ev.Index)
			args := strings.TrimSpace(b.args.String())
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				out <- Chunk{Type: ChunkError, Err: fmt.Errorf(
					"tool %q arguments were not valid JSON: %s", b.name, truncateArgs(args))}
				continue
			}
			out <- Chunk{Type: ChunkToolCall, ToolCall: &ToolCall{
				ID: b.id, Name: b.name, Args: json.RawMessage(args),
			}}

		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stop = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				usage.OutputTokens += ev.Usage.OutputTokens
			}

		case "message_stop":
			out <- Chunk{Type: ChunkDone, Usage: &usage, StopReason: stop}
			return

		case "error":
			if ev.Error != nil {
				out <- Chunk{Type: ChunkError, Err: fmt.Errorf("anthropic: %s: %s",
					ev.Error.Type, ev.Error.Message)}
			}
			return
		}

		if ctx.Err() != nil {
			return
		}
	}
	if err := sc.Err(); err != nil {
		out <- Chunk{Type: ChunkError, Err: fmt.Errorf("stream read failed: %w", err)}
		return
	}
	// The stream ended without message_stop: report what was counted rather
	// than leaving the loop with no terminal chunk.
	out <- Chunk{Type: ChunkDone, Usage: &usage, StopReason: stop}
}

// CountTokens estimates prompt size.
//
// The Messages API has a real counting endpoint, but calling it costs a round
// trip on every compaction check — several per session — for a number that only
// decides when to summarize. The same character-based estimate the other
// adapters use is close enough for that decision and free.
func (c *Anthropic) CountTokens(req Request) (int, error) {
	return estimateTokens(req), nil
}

func truncateArgs(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "…"
}

// betaHeader adds a required beta flag to whatever the operator configured,
// without duplicating it if they already named it.
func betaHeader(configured []string, required string) string {
	for _, b := range configured {
		if b == required {
			return strings.Join(configured, ",")
		}
	}
	return strings.Join(append([]string{required}, configured...), ",")
}

// errSubscriptionRestricted explains a refusal that arrives dressed as a rate
// limit.
//
// A Claude Pro or Max subscription token is accepted by the API and then
// refused for anything but Anthropic's own apps. The refusal comes back as 429
// with a rate_limit_error, so without this the user is told to wait for a limit
// that will never clear — and Abhed dutifully retries four times against a wall.
//
// Saying so plainly costs one paragraph and saves an afternoon. The condition
// is deliberately narrow: only a bearer token, only a 429 that carries none of
// the headers a real rate limit carries.
var errSubscriptionRestricted = fmt.Errorf( //nolint:staticcheck // a multi-line message shown to a person, laid out on purpose
	"this subscription token was refused (HTTP 429, with none of the headers a " +
		"rate limit carries).\n" +
		"  A Claude Pro or Max token is restricted to Anthropic's own apps: Anthropic checks " +
		"the system prompt and refuses other clients.\n" +
		"  Use an API key from platform.claude.com instead:\n" +
		"      unset CLAUDE_CODE_OAUTH_TOKEN\n" +
		"      export ANTHROPIC_API_KEY=sk-ant-api03-...")

// fatalStatus stops the retry loop for a failure that will not clear.
//
// Only this adapter can make the call, and only when a subscription token is in
// use: a 429 with no reset information is ambiguous in general — plenty of real
// rate limiters omit those headers — but from a bearer token it is the shape of
// Anthropic refusing a client that is not its own, and waiting for it is
// waiting for something that will never happen.
func (c *Anthropic) fatalStatus(se *StatusError) bool {
	return c.Bearer != "" && looksLikeSubscriptionRefusal(se)
}

// looksLikeSubscriptionRefusal distinguishes the refusal from a real rate
// limit.
//
// A genuine limit reports when it resets — Retry-After, or the
// anthropic-ratelimit headers — and says something specific. This one carries
// neither and its message is the literal string "Error". Requiring the absence
// of those headers is what keeps a real rate limit from being misreported as a
// policy refusal, which would be the worse mistake of the two.
func looksLikeSubscriptionRefusal(se *StatusError) bool {
	if se.Status != http.StatusTooManyRequests {
		return false
	}
	if se.RetryAfter || se.HasRateLimitHeaders {
		return false
	}
	return strings.Contains(se.Body, "rate_limit_error")
}
