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

// Gemini speaks Google's generateContent API.
//
// A third wire shape, and the one that agrees with the others least. Messages
// are "contents" with a "parts" list; the assistant role is called "model";
// tool calls are functionCall parts and results are functionResponse parts
// carrying an object rather than a string; the system prompt is
// systemInstruction; and sampling lives in a generationConfig sub-object.
//
// Google also publishes an OpenAI-compatible endpoint. It is deliberately not
// what this uses: that shim drops thinking output and the function-calling
// controls, which are most of the reason to reach for Gemini in an agent.
type Gemini struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client

	// Defaults are the operator's configured sampling parameters, overridden
	// per request by anything the request itself sets.
	Defaults Params

	// Retry bounds how long a transient failure is waited out, and Notify
	// reports one to the user.
	Retry  RetryPolicy
	Notify func(string)

	profile Profile
}

func NewGemini(baseURL, apiKey, model string, p Profile) *Gemini {
	if p.Name == "" {
		p.Name = model
	}
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	return &Gemini{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: 10 * time.Minute},
		Retry:   DefaultRetry(),
		profile: p,
	}
}

func (g *Gemini) Name() string     { return g.profile.Name }
func (g *Gemini) Profile() Profile { return g.profile }

type geminiPart struct {
	Text             string          `json:"text,omitempty"`
	FunctionCall     *geminiCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiResponse `json:"functionResponse,omitempty"`
	Thought          bool            `json:"thought,omitempty"`
	// InlineData carries an image. Gemini takes base64 with a mime type.
	InlineData *geminiBlob `json:"inlineData,omitempty"`
}

type geminiCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

// geminiBlob is an inline image.
type geminiBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiGenConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	TopK            *int     `json:"topK,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
	Seed            *int64   `json:"seed,omitempty"`

	ThinkingConfig *geminiThinking `json:"thinkingConfig,omitempty"`
}

type geminiThinking struct {
	ThinkingBudget  *int `json:"thinkingBudget,omitempty"`
	IncludeThoughts bool `json:"includeThoughts,omitempty"`
}

type geminiRequest struct {
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	Tools             []geminiToolset  `json:"tools,omitempty"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
}

type geminiToolset struct {
	FunctionDeclarations []geminiFunc `json:"functionDeclarations"`
}

type geminiFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// geminiParts renders one message's content, staying a single text part when
// that is all the message holds.
func geminiParts(m Message) []geminiPart {
	if len(m.Blocks) == 0 {
		return []geminiPart{{Text: m.Content}}
	}
	out := make([]geminiPart, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		switch b.Kind {
		case BlockText:
			if b.Text != "" {
				out = append(out, geminiPart{Text: b.Text})
			}
		case BlockImage:
			out = append(out, geminiPart{InlineData: &geminiBlob{
				MimeType: b.MediaType,
				Data:     base64.StdEncoding.EncodeToString(b.Data),
			}})
		}
	}
	if len(out) == 0 {
		return []geminiPart{{Text: m.Content}}
	}
	return out
}

func (g *Gemini) buildRequest(req Request) geminiRequest {
	sp := g.Defaults.Merge(req.Sampling())

	var contents []geminiContent
	for _, m := range req.Messages {
		switch m.Role {
		case RoleTool:
			// functionResponse carries an object, so a plain string result has
			// to be wrapped rather than sent as-is.
			payload, err := json.Marshal(map[string]string{"result": m.Content})
			if err != nil {
				payload = []byte(`{"result":""}`)
			}
			contents = append(contents, geminiContent{
				Role: "user",
				Parts: []geminiPart{{FunctionResponse: &geminiResponse{
					Name: m.ToolCallID, Response: payload,
				}}},
			})
		case RoleAssistant:
			var parts []geminiPart
			if strings.TrimSpace(m.Content) != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				args := tc.Args
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				parts = append(parts, geminiPart{FunctionCall: &geminiCall{
					Name: tc.Name, Args: args,
				}})
			}
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, geminiContent{Role: "model", Parts: parts})
		default:
			contents = append(contents, geminiContent{
				Role: "user", Parts: geminiParts(m),
			})
		}
	}

	out := geminiRequest{Contents: contents}
	if req.System != "" {
		out.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: req.System}}}
	}

	var decls []geminiFunc
	for _, t := range req.Tools {
		decls = append(decls, geminiFunc{
			Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
		})
	}
	if len(decls) > 0 {
		out.Tools = []geminiToolset{{FunctionDeclarations: decls}}
	}

	cfg := geminiGenConfig{
		Temperature:     sp.Temperature,
		TopP:            sp.TopP,
		TopK:            sp.TopK,
		MaxOutputTokens: sp.MaxTokens,
		StopSequences:   sp.Stop,
		Seed:            sp.Seed,
	}
	if sp.ThinkingBudget != nil {
		cfg.ThinkingConfig = &geminiThinking{
			ThinkingBudget: sp.ThinkingBudget, IncludeThoughts: true,
		}
	} else if sp.Think != nil && *sp.Think {
		cfg.ThinkingConfig = &geminiThinking{IncludeThoughts: true}
	}
	out.GenerationConfig = &cfg
	return out
}

type geminiStreamChunk struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount        int  `json:"promptTokenCount"`
		CandidatesTokenCount    int  `json:"candidatesTokenCount"`
		CachedContentTokenCount *int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (g *Gemini) Complete(ctx context.Context, req Request) (<-chan Chunk, error) {
	// Refuse an image the endpoint cannot read, rather than sending it and
	// letting the provider 400 with its own wording — or silently ignore it.
	if err := CheckVision(g.Profile(), req); err != nil {
		return nil, err
	}
	body, err := json.Marshal(g.buildRequest(req))
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// alt=sse asks for server-sent events; without it the endpoint returns a
	// single JSON array at the end, which is not streaming.
	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", g.BaseURL, g.Model)
	newRequest := func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if g.APIKey != "" {
			httpReq.Header.Set("x-goog-api-key", g.APIKey)
		}
		return httpReq, nil
	}

	resp, err := send(ctx, g.HTTP, g.Retry, newRequest, g.Notify, g.fatalStatus)
	if err != nil {
		se := &StatusError{}
		if errors.As(err, &se) {
			return nil, fmt.Errorf("gemini returned %s", se.Error())
		}
		return nil, fmt.Errorf("%s is unreachable: %w", g.BaseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("gemini returned %s: %s", resp.Status,
			strings.TrimSpace(string(msg)))
	}

	out := make(chan Chunk, 32)
	go g.stream(ctx, resp, out)
	return out, nil
}

func (g *Gemini) stream(ctx context.Context, resp *http.Response, out chan<- Chunk) {
	defer close(out)
	defer resp.Body.Close()

	usage := Usage{}
	stop := ""
	calls := 0

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var chunk geminiStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			out <- Chunk{Type: ChunkError, Err: fmt.Errorf("gemini: %s", chunk.Error.Message)}
			return
		}
		if u := chunk.UsageMetadata; u != nil {
			// Gemini reports cumulative totals per chunk, so these are
			// assigned rather than accumulated.
			usage.InputTokens = u.PromptTokenCount
			usage.OutputTokens = u.CandidatesTokenCount
			if u.CachedContentTokenCount != nil {
				usage.CachedInputTokens, usage.CacheReported = *u.CachedContentTokenCount, true
			}
		}

		for _, cand := range chunk.Candidates {
			if cand.FinishReason != "" {
				stop = cand.FinishReason
			}
			for _, part := range cand.Content.Parts {
				switch {
				case part.FunctionCall != nil:
					args := part.FunctionCall.Args
					if len(args) == 0 {
						args = json.RawMessage("{}")
					}
					calls++
					out <- Chunk{Type: ChunkToolCall, ToolCall: &ToolCall{
						// Gemini does not issue call ids; the loop needs one to
						// pair the result back, so synthesize a stable one.
						ID:   fmt.Sprintf("%s-%d", part.FunctionCall.Name, calls),
						Name: part.FunctionCall.Name,
						Args: args,
					}}
				case part.Thought:
					if part.Text != "" {
						out <- Chunk{Type: ChunkReasoning, Text: part.Text}
					}
				case part.Text != "":
					out <- Chunk{Type: ChunkText, Text: part.Text}
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
	if err := sc.Err(); err != nil {
		out <- Chunk{Type: ChunkError, Err: fmt.Errorf("stream read failed: %w", err)}
		return
	}
	out <- Chunk{Type: ChunkDone, Usage: &usage, StopReason: stop}
}

func (g *Gemini) CountTokens(req Request) (int, error) { return estimateTokens(req), nil }

// fatalStatus: this adapter has no status it can recognise as hopeless, so
// every retryable failure is retried.
func (g *Gemini) fatalStatus(*StatusError) bool { return false }
