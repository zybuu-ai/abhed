package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
)

// summarizerAdapter returns a fixed summary and records what it was asked.
type summarizerAdapter struct {
	summary   string
	window    int
	sawPrompt string
	calls     int
}

func (s *summarizerAdapter) Name() string { return "summarizer" }
func (s *summarizerAdapter) Profile() model.Profile {
	return model.Profile{ContextWindow: s.window}
}

// CountTokens uses a simple char/4 estimate so tests can drive the threshold.
func (s *summarizerAdapter) CountTokens(req model.Request) (int, error) {
	n := len(req.System)
	for _, m := range req.Messages {
		n += len(m.Content)
	}
	return n / 4, nil
}

func (s *summarizerAdapter) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	s.calls++
	if len(req.Messages) > 0 {
		s.sawPrompt = req.Messages[0].Content
	}
	ch := make(chan model.Chunk, 4)
	ch <- model.Chunk{Type: model.ChunkText, Text: s.summary}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
	close(ch)
	return ch, nil
}

func longMessages(n int, size int) []model.Message {
	msgs := make([]model.Message, 0, n)
	for i := 0; i < n; i++ {
		role := model.RoleUser
		if i%2 == 1 {
			role = model.RoleAssistant
		}
		msgs = append(msgs, model.Message{Role: role, Content: strings.Repeat("x", size)})
	}
	return msgs
}

func TestShouldCompactAtThreshold(t *testing.T) {
	a := &summarizerAdapter{window: 1000, summary: "s"}
	c := NewCompactor(a, 0.90)

	small := longMessages(2, 100) // ~50 tokens
	should, _, err := c.ShouldCompact("", small, nil)
	if err != nil {
		t.Fatal(err)
	}
	if should {
		t.Fatal("should not compact a small conversation")
	}

	big := longMessages(40, 400) // ~4000 tokens, well over 900
	should, used, err := c.ShouldCompact("", big, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !should {
		t.Fatalf("should compact at %d tokens against a 1000-token window", used)
	}
}

func TestCompactPreservesRecentTurns(t *testing.T) {
	a := &summarizerAdapter{window: 1000, summary: "SUMMARY OF EARLIER WORK"}
	c := NewCompactor(a, 0.90)
	c.KeepRecentTurns = 2

	msgs := []model.Message{
		{Role: model.RoleUser, Content: "first task"},
		{Role: model.RoleAssistant, Content: "did first"},
		{Role: model.RoleUser, Content: "second task"},
		{Role: model.RoleAssistant, Content: "did second"},
		{Role: model.RoleUser, Content: "third task"},
		{Role: model.RoleAssistant, Content: "did third"},
	}

	out, info, err := c.Compact(context.Background(), "auto", "sys", msgs, 900)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out[0].Content, "SUMMARY OF EARLIER WORK") {
		t.Fatalf("first message should carry the summary, got %q", out[0].Content)
	}
	// The most recent turns must survive verbatim.
	last := out[len(out)-1]
	if last.Content != "did third" {
		t.Fatalf("recent turns must be preserved, got %q", last.Content)
	}
	if info.Trigger != "auto" {
		t.Fatalf("trigger not recorded: %q", info.Trigger)
	}
	if info.BeforeTokens != 900 {
		t.Fatalf("before-token accounting lost: %d", info.BeforeTokens)
	}
}

// Compaction must not split an assistant's tool calls from their results.
func TestCompactKeepsToolCallsWithResults(t *testing.T) {
	a := &summarizerAdapter{window: 1000, summary: "s"}
	c := NewCompactor(a, 0.90)
	c.KeepRecentTurns = 1

	msgs := []model.Message{
		{Role: model.RoleUser, Content: "old"},
		{Role: model.RoleAssistant, Content: "old reply"},
		{Role: model.RoleUser, Content: "run the tests"},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: model.RoleTool, ToolCallID: "c1", Content: "exit 0"},
	}

	out, _, err := c.Compact(context.Background(), "auto", "", msgs, 900)
	if err != nil {
		t.Fatal(err)
	}
	// Every tool result in the output must have its call present.
	callIDs := map[string]bool{}
	for _, m := range out {
		for _, tc := range m.ToolCalls {
			callIDs[tc.ID] = true
		}
	}
	for _, m := range out {
		if m.Role == model.RoleTool && m.ToolCallID != "" && !callIDs[m.ToolCallID] {
			t.Fatalf("orphaned tool result %s: its call was summarized away", m.ToolCallID)
		}
	}
}

func TestPreCompactHookRuns(t *testing.T) {
	a := &summarizerAdapter{window: 1000, summary: "s"}
	c := NewCompactor(a, 0.90)

	var gotTrigger string
	var gotCount int
	c.PreCompact = func(trigger string, msgs []model.Message) error {
		gotTrigger = trigger
		gotCount = len(msgs)
		return nil
	}

	msgs := longMessages(10, 100)
	if _, _, err := c.Compact(context.Background(), "manual", "", msgs, 500); err != nil {
		t.Fatal(err)
	}
	if gotTrigger != "manual" {
		t.Fatalf("hook should receive the trigger, got %q", gotTrigger)
	}
	if gotCount != len(msgs) {
		t.Fatalf("hook should see the full transcript before it is discarded, got %d of %d", gotCount, len(msgs))
	}
}

func TestSummarizerNotGivenTools(t *testing.T) {
	a := &summarizerAdapter{window: 1000, summary: "s"}
	c := NewCompactor(a, 0.90)
	c.KeepRecentTurns = 1
	// Needs more turns than KeepRecentTurns, or there is nothing older to
	// summarize and Compact correctly returns early.
	msgs := longMessages(12, 100)

	if _, _, err := c.Compact(context.Background(), "auto", "", msgs, 500); err != nil {
		t.Fatal(err)
	}
	if a.calls == 0 {
		t.Fatal("summarizer was never called")
	}
	// The summarizer sees a rendered transcript, not replayable tool schemas -
	// otherwise it would try to call them.
	if !strings.Contains(a.sawPrompt, "Summarize the conversation") {
		t.Fatalf("summarizer prompt not used: %q", truncate(a.sawPrompt, 80))
	}
}

func TestCompactionSurfacesTokenReduction(t *testing.T) {
	a := &summarizerAdapter{window: 1000, summary: "short summary"}
	c := NewCompactor(a, 0.90)
	msgs := longMessages(20, 400)

	_, info, err := c.Compact(context.Background(), "auto", "sys", msgs, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if info.AfterTokens >= info.BeforeTokens {
		t.Fatalf("compaction should reduce tokens: %d -> %d", info.BeforeTokens, info.AfterTokens)
	}
	t.Logf("compacted %d -> %d tokens (%.0f%% reduction)",
		info.BeforeTokens, info.AfterTokens,
		100*(1-float64(info.AfterTokens)/float64(info.BeforeTokens)))
}

func TestNoCompactionWhenWindowUnknown(t *testing.T) {
	a := &summarizerAdapter{window: 0, summary: "s"}
	c := NewCompactor(a, 0.90)
	should, _, err := c.ShouldCompact("", longMessages(100, 1000), nil)
	if err != nil {
		t.Fatal(err)
	}
	if should {
		t.Fatal("must not auto-compact when the context window is unknown")
	}
}

func TestPrefillSavingsMetric(t *testing.T) {
	u := Usage{InputTokens: 100000, CachedTokens: 94000, ColdPrefillTokens: 6000}
	if got := u.PrefillSavings(); got < 16 || got > 17.5 {
		t.Fatalf("expected ~16.7x savings, got %.1f", got)
	}
	if rate := u.CacheHitRate(); rate < 0.93 || rate > 0.95 {
		t.Fatalf("cache hit rate wrong: %.2f", rate)
	}
}

// A manual compaction's focus reaches the summarizer once; an automatic
// compaction never takes it.
func TestCompactFocusReachesTheSummarizer(t *testing.T) {
	a := &summarizerAdapter{window: 100000, summary: "s"}
	c := NewCompactor(a, 0.90)
	c.SetFocus("keep the API names")
	if _, _, err := c.Compact(context.Background(), "auto", "", longMessages(20, 10), 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(a.sawPrompt, "keep the API names") {
		t.Fatal("an automatic compaction took the person's focus")
	}
	if _, _, err := c.Compact(context.Background(), "manual", "", longMessages(20, 10), 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.sawPrompt, "keep, above all else: keep the API names") {
		t.Fatalf("the focus did not reach the summarizer:\n%s", a.sawPrompt)
	}
	if _, _, err := c.Compact(context.Background(), "manual", "", longMessages(20, 10), 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(a.sawPrompt, "keep the API names") {
		t.Fatal("the focus was used twice")
	}
}
