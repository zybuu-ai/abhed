// Package model abstracts over reasoning models.
//
// Everything model-specific lives behind the Adapter interface: tool-call
// parsing, reasoning-token handling, chat templates, structured-output
// strategy. If you find yourself forking the *prompt* per model, the
// difference belongs here instead (docs §07 anti-patterns).
package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn of conversation. ToolCalls and ToolResults are kept
// structured rather than flattened into text so adapters can render them in
// whatever format their model family expects.
type Message struct {
	Role    Role
	Content string
	// Blocks carries typed content — text and images — for messages that are
	// not just a string.
	//
	// Additive on purpose. Content stays the one field every existing caller
	// writes and reads, so the two dozen places that construct a Message and
	// every consumer that renders one keep working untouched. Only the four
	// adapters and the token estimator look here. Replacing Content with a
	// block list would have been the tidier type and a far riskier change for
	// no gain to the callers that only ever have text.
	//
	// When Blocks is set it is authoritative and Content is a plain-text
	// rendering of it, so anything that only understands text still reads
	// something sensible rather than an empty turn.
	Blocks     []ContentBlock
	ToolCalls  []ToolCall
	ToolCallID string // set when Role == RoleTool
	IsError    bool   // tool result was an error
}

// BlockKind distinguishes the kinds of content a message can carry.
type BlockKind string

const (
	BlockText  BlockKind = "text"
	BlockImage BlockKind = "image"
)

// ContentBlock is one piece of a multimodal message.
//
// Images are carried as raw bytes plus a media type rather than a URL. A URL
// would make the model provider fetch it, which means the bytes leave the
// network on a deployment whose entire premise is that they do not — and it
// would let a prompt name an internal address for the provider to retrieve.
type ContentBlock struct {
	Kind BlockKind
	Text string
	// Data is the decoded image. Adapters base64 it as their wire format
	// requires; keeping it decoded here means it is encoded once per request
	// rather than carried encoded and re-decoded to measure.
	Data      []byte
	MediaType string // image/png, image/jpeg, image/gif, image/webp
}

// TextOf renders a message as plain text, whatever it carries.
//
// The single place that decides what an image "reads as" for a consumer that
// cannot show one — the compaction summariser, the SDK's last-message helper,
// the extension bridge. Without it each of those would invent its own
// placeholder and they would drift.
func (m Message) TextOf() string {
	if len(m.Blocks) == 0 {
		return m.Content
	}
	var b strings.Builder
	for _, blk := range m.Blocks {
		switch blk.Kind {
		case BlockText:
			b.WriteString(blk.Text)
		case BlockImage:
			b.WriteString("[image: " + blk.MediaType + "]")
		}
	}
	return b.String()
}

// HasImages reports whether this message needs a vision-capable model.
func (m Message) HasImages() bool {
	for _, b := range m.Blocks {
		if b.Kind == BlockImage {
			return true
		}
	}
	return false
}

type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// EffortLevel controls reasoning budget. Per docs P11 this is a measured,
// per-model, per-task-class parameter — never assume more is better.
type EffortLevel string

const (
	EffortNone   EffortLevel = ""
	EffortLow    EffortLevel = "low"
	EffortMedium EffortLevel = "medium"
	EffortHigh   EffortLevel = "high"
)

type Request struct {
	System   string // the cached prefix (docs §07 layers 1-4)
	Messages []Message
	Tools    []ToolDef
	// Params carries sampling and decoding controls. MaxTokens, Stop and
	// Effort live here too; the older top-level fields are kept as the
	// per-request override that composes over the configured defaults.
	Params Params

	MaxTokens   int
	Effort      EffortLevel
	Stop        []string
	Temperature *float64
}

// Sampling resolves the effective parameters for this request: the configured
// Params, with any per-request field set on the legacy top-level fields taking
// precedence.
func (r Request) Sampling() Params {
	out := r.Params
	if r.MaxTokens != 0 {
		out.MaxTokens = r.MaxTokens
	}
	if r.Effort != EffortNone {
		out.Effort = r.Effort
	}
	if len(r.Stop) > 0 {
		out.Stop = r.Stop
	}
	if r.Temperature != nil {
		out.Temperature = r.Temperature
	}
	return out
}

// ChunkType distinguishes the pieces of a streamed response.
type ChunkType string

const (
	ChunkText      ChunkType = "text"
	ChunkReasoning ChunkType = "reasoning" // never fed back as tool input
	ChunkToolCall  ChunkType = "tool_call"
	ChunkDone      ChunkType = "done"
	ChunkError     ChunkType = "error"
)

type Chunk struct {
	Type       ChunkType
	Text       string
	ToolCall   *ToolCall
	Usage      *Usage
	Err        error
	StopReason string
}

type Usage struct {
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int // drives the cache-hit metric (docs P8)
	// CacheReported is set when the provider sent a cached-token figure, zero
	// included; some endpoints send none, and absence is not a cold cache.
	CacheReported   bool
	ReasoningTokens int
}

// Profile describes what a model can do. Populated by the conformance suite
// at registration time (docs §08 L2) and consulted by the harness.
type Profile struct {
	Name            string `json:"name"`
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	SupportsTools   bool   `json:"supports_tools"`
	SupportsStream  bool   `json:"supports_stream"`
	// SupportsVision reports whether this endpoint accepts image content.
	//
	// Defaults false, which is the safe answer for an unknown endpoint: an
	// image sent to a text-only server is a 400 at best and silently ignored
	// at worst, and "the model did not mention the screenshot" is a much
	// harder failure to diagnose than a clear refusal.
	SupportsVision  bool               `json:"supports_vision"`
	ToolCallFormat  string             `json:"tool_call_format"` // json | xml | pythonic | harmony
	ReasoningTokens bool               `json:"reasoning_tokens"`
	GuidedDecoding  bool               `json:"guided_decoding"`
	CachePrefix     bool               `json:"cache_prefix"`
	Conformance     map[string]float64 `json:"conformance,omitempty"`
	// Sampling reports which decoding knobs this provider honours, so a
	// config naming one it does not is rejected rather than ignored.
	Sampling Sampling `json:"-"`
}

// Adapter is the single seam that makes Abhed model-agnostic.
type Adapter interface {
	Name() string
	Profile() Profile
	// Complete streams a response. The channel closes when the turn ends.
	Complete(ctx context.Context, req Request) (<-chan Chunk, error)
	// CountTokens estimates prompt size for budget and compaction decisions.
	CountTokens(req Request) (int, error)
}

// estimateTokens approximates prompt size from character counts.
//
// Every adapter needs this for the same decision — when to compact — and none
// of them needs it to be exact. A real tokenizer would mean shipping vocabulary
// files per model family, and the counting endpoints that exist cost a round
// trip on a question asked several times a session. An estimate that is close
// and free is the right trade for a threshold check; the model reports true
// usage afterwards, which is what the budget is actually reconciled against.
// imageTokenEstimate is what one image costs, in tokens.
//
// A deliberate over-estimate of a typical screenshot. Compaction has to fire
// BEFORE the window is full — overshooting wastes a little context, while
// undershooting fails a turn outright with no way to recover it.
const imageTokenEstimate = 1600

func estimateTokens(req Request) int {
	n := len(req.System)
	images := 0
	for _, m := range req.Messages {
		// An image is NOT its byte length. A 2 MB PNG is ~2.7M base64
		// characters, which this would report as ~750k tokens — enough to
		// trigger compaction on every turn and eventually to refuse a request
		// that in truth costs about 1,500. Providers price an image by its
		// dimensions (Anthropic bills roughly w*h/750), so blocks are counted
		// separately at a flat estimate rather than by size.
		if len(m.Blocks) > 0 {
			for _, b := range m.Blocks {
				switch b.Kind {
				case BlockText:
					n += len(b.Text)
				case BlockImage:
					images++
				}
			}
			n += 16
		} else {
			n += len(m.Content) + 16 // per-message framing overhead
		}
		for _, tc := range m.ToolCalls {
			n += len(tc.Name) + len(tc.Args) + 16
		}
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	// ~3.6 chars/token is a reasonable average for code-heavy English text.
	// Images are added after the character conversion because they were never
	// characters: imageTokenEstimate is already a token count.
	return n*10/36 + images*imageTokenEstimate
}

// ErrVisionUnsupported is returned when a request carries an image and the
// endpoint cannot see.
//
// Loud on purpose. A text-only server given an image either 400s with a
// provider-specific message or, worse, ignores the block and answers as if it
// had looked — and "the model did not mention the screenshot" is a far harder
// thing to diagnose than a refusal that names the cause.
var ErrVisionUnsupported = errors.New(
	"this model cannot read images; choose a vision-capable provider or remove the attachment")

// CheckVision reports whether a request can be served by a profile.
//
// Called by adapters before a request goes out, so the check lives once beside
// the flag rather than being re-implemented per adapter with slightly different
// wording each time.
func CheckVision(p Profile, req Request) error {
	if p.SupportsVision {
		return nil
	}
	for _, m := range req.Messages {
		if m.HasImages() {
			return ErrVisionUnsupported
		}
	}
	return nil
}
