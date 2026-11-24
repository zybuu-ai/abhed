package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// growingAdapter simulates a real session: each turn adds a tool call whose
// result is large, so history grows the way it does in practice.
type growingAdapter struct {
	dir        string
	turns      int
	window     int
	summarized int
	maxSeen    int
}

func (g *growingAdapter) Name() string { return "growing" }
func (g *growingAdapter) Profile() model.Profile {
	return model.Profile{ContextWindow: g.window}
}
func (g *growingAdapter) CountTokens(req model.Request) (int, error) {
	n := len(req.System)
	for _, m := range req.Messages {
		n += len(m.Content) + 16
		for _, tc := range m.ToolCalls {
			n += len(tc.Name) + len(tc.Args) + 16
		}
	}
	return n * 10 / 36, nil
}

func (g *growingAdapter) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	used, _ := g.CountTokens(req)
	if used > g.maxSeen {
		g.maxSeen = used
	}
	// A real endpoint rejects a request over its window. Simulating that is the
	// whole point: without it, a harness that never compacts looks fine.
	if used > g.window {
		return nil, fmt.Errorf("context window exceeded: %d > %d", used, g.window)
	}

	ch := make(chan model.Chunk, 8)
	// The summarizer calls with no tools; count those separately.
	if len(req.Tools) == 0 {
		g.summarized++
		ch <- model.Chunk{Type: model.ChunkText, Text: "Summary of prior work: did things."}
		ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
		close(ch)
		return ch, nil
	}

	g.turns++
	if g.turns >= 100 {
		ch <- model.Chunk{Type: model.ChunkText, Text: "done after 100 turns"}
		ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
		close(ch)
		return ch, nil
	}
	ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &model.ToolCall{
		ID: fmt.Sprintf("c%d", g.turns), Name: "read",
		Args: []byte(fmt.Sprintf(`{"path":%q}`, filepath.Join(g.dir, fmt.Sprintf("file%d.txt", ((g.turns-1)%100)+1)))),
	}}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
	close(ch)
	return ch, nil
}

// Can Abhed actually sustain 100 turns inside a fixed window?
func strings_Repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func TestHundredTurnSession(t *testing.T) {
	dir := tempDir(t)
	// Seed a file whose contents are large, so each tool result grows history.
	// Many lines, each under the per-line cap, so the Read tool returns the
	// whole thing — a realistic several-thousand-token tool result.
	var sb []byte
	for i := 0; i < 400; i++ {
		sb = append(sb, []byte(fmt.Sprintf("line %03d: %s\n", i, strings_Repeat("data ", 20)))...)
	}
	big := sb
	for i := 1; i <= 100; i++ {
		path := filepath.Join(dir, fmt.Sprintf("file%d.txt", i))
		if err := os.WriteFile(path, big, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ad := &growingAdapter{window: 32768, dir: dir}
	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	rec := NewRecorder(store, "sess1", "")
	reg := tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Edit{},
		tools.Glob{}, tools.Grep{}, tools.Bash{})
	cfg := DefaultConfig()
	cfg.MaxTurns = 120
	loop := NewLoop(ad, reg, policy.New(policy.ModeAuto),
		AutoApprove{Yes: true}, sess, rec, cfg)
	loop.Compactor = NewCompactor(ad, 0.80)

	reason, runErr := loop.Run(context.Background(), "read every file")
	t.Logf("terminal=%v err=%v turns=%d compactions=%d peakTokens=%d window=%d",
		reason, runErr, loop.Usage().Turns, loop.Usage().Compactions, ad.maxSeen, ad.window)

	evs, _ := store.Events("sess1")
	nCompact := 0
	for _, e := range evs {
		if e.Type == EvCompactDone {
			nCompact++
		}
	}
	t.Logf("compaction events: %d", nCompact)

	if runErr != nil {
		t.Fatalf("a 100-turn session must not fail: %v", runErr)
	}
	if loop.Usage().Turns < 100 {
		t.Errorf("reached only %d turns, want 100", loop.Usage().Turns)
	}
}

func TestBoundaryBeforeOnAgentShapedHistory(t *testing.T) {
	msgs := []model.Message{{Role: model.RoleUser, Content: "do the thing"}}
	for i := 0; i < 20; i++ {
		msgs = append(msgs,
			model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c", Name: "read"}}},
			model.Message{Role: model.RoleTool, ToolCallID: "c", Content: "result"})
	}
	split := boundaryBefore(msgs, 4)
	t.Logf("history=%d messages, split=%d, older=%d recent=%d",
		len(msgs), split, split, len(msgs)-split)
	if split == 0 {
		t.Fatalf("split=0: nothing is ever summarized, so compaction is a no-op " +
			"on exactly the shape a long agent session has")
	}
}

func TestCompactActuallyShrinksAgentHistory(t *testing.T) {
	ad := &growingAdapter{window: 32768}
	c := NewCompactor(ad, 0.80)

	// Four exchanges, the shape at the moment compaction is needed.
	msgs := []model.Message{{Role: model.RoleUser, Content: "do it"}}
	for i := 0; i < 4; i++ {
		msgs = append(msgs,
			model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c", Name: "read"}}},
			model.Message{Role: model.RoleTool, ToolCallID: "c", Content: strings_Repeat("y", 29000)})
	}
	before, _ := ad.CountTokens(model.Request{Messages: msgs})
	split := boundaryBefore(msgs, c.KeepRecentTurns)
	t.Logf("msgs=%d before=%d KeepRecentTurns=%d split=%d", len(msgs), before, c.KeepRecentTurns, split)

	out, info, err := c.Compact(unmetered(), "auto", "", msgs, before)
	t.Logf("after: msgs=%d tokens=%d err=%v", len(out), info.AfterTokens, err)
	if len(out) == len(msgs) {
		t.Fatalf("Compact was a no-op: keeping %d exchanges leaves nothing older to "+
			"summarize, so a session at the threshold can never shrink",
			c.KeepRecentTurns)
	}
}

func TestCompactionIntervalOnRealisticSession(t *testing.T) {
	dir := tempDir(t)
	small := []byte(strings_Repeat("short result line\n", 20))         // ~100 tokens
	large := []byte(strings_Repeat("data data data data data\n", 800)) // ~5.5k tokens
	for i := 1; i <= 100; i++ {
		body := small
		if i%10 == 0 {
			body = large // one big read in ten
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%d.txt", i)), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ad := &growingAdapter{window: 32768, dir: dir}
	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	rec := NewRecorder(store, "sess1", "")
	reg := tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Edit{}, tools.Glob{}, tools.Grep{}, tools.Bash{})
	cfg := DefaultConfig()
	cfg.MaxTurns = 120
	loop := NewLoop(ad, reg, policy.New(policy.ModeAuto), AutoApprove{Yes: true}, sess, rec, cfg)
	loop.Compactor = NewCompactor(ad, 0.80)

	reason, runErr := loop.Run(context.Background(), "read the files")
	t.Logf("terminal=%v err=%v turns=%d compactions=%d peak=%d window=%d",
		reason, runErr, loop.Usage().Turns, loop.Usage().Compactions, ad.maxSeen, ad.window)

	if runErr != nil {
		t.Fatalf("session failed: %v", runErr)
	}
	if loop.Usage().Turns < 100 {
		t.Errorf("turns = %d, want 100", loop.Usage().Turns)
	}
	// Each compaction is a summarizer call and a discarded prefix cache. On a
	// session whose turns are mostly small, they should be occasional.
	if n := loop.Usage().Compactions; n > 12 {
		t.Errorf("compactions = %d over 100 turns; too frequent for this workload", n)
	}
}
