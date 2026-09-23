// Command abhed-bench measures prefix-cache behavior on a real serving stack.
//
// The capacity model in docs/architecture/04-sizing.md rests on a COMPUTED claim
// that prefix caching is worth ~17x on a long session — and on the corollary
// that compaction invalidates the prefix and pays cold prefill again.
//
// Neither is verified by the research pass. This measures both against whatever
// endpoint you point it at, so the number in your capacity plan is yours rather
// than an assumption.
//
//	abhed-bench -base-url http://gpu:8000/v1 -model Qwen/Qwen3-32B
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
)

type turnResult struct {
	Turn         int           `json:"turn"`
	PromptTokens int           `json:"prompt_tokens"`
	CachedTokens int           `json:"cached_tokens"`
	ColdTokens   int           `json:"cold_tokens"`
	TTFT         time.Duration `json:"ttft_ns"`
	Total        time.Duration `json:"total_ns"`
	reported     bool          // the endpoint sent a cached-token figure, zero included
}

type report struct {
	Endpoint      string        `json:"endpoint"`
	Model         string        `json:"model"`
	PrefixTokens  int           `json:"prefix_tokens_est"`
	Turns         []turnResult  `json:"turns"`
	CacheReported bool          `json:"cache_reported"`
	Summary       summaryStats  `json:"summary"`
	Compaction    *compactStats `json:"compaction,omitempty"`
}

type summaryStats struct {
	TotalPrompt   int     `json:"total_prompt_tokens"`
	TotalCached   int     `json:"total_cached_tokens"`
	TotalCold     int     `json:"total_cold_tokens"`
	HitRate       float64 `json:"cache_hit_rate"`
	PrefillFactor float64 `json:"prefill_savings_factor"`
	ColdTTFTMs    float64 `json:"cold_ttft_ms"`
	WarmTTFTMs    float64 `json:"warm_ttft_ms"`
	TTFTSpeedup   float64 `json:"ttft_speedup"`
}

type compactStats struct {
	TTFTBeforeMs float64 `json:"ttft_before_ms"`
	TTFTAfterMs  float64 `json:"ttft_after_ms"`
	Penalty      float64 `json:"cold_penalty_factor"`
	Note         string  `json:"note"`
}

func main() {
	var (
		baseURL     = flag.String("base-url", envOr("ABHED_BASE_URL", "http://localhost:8000/v1"), "OpenAI-compatible endpoint")
		modelName   = flag.String("model", envOr("ABHED_MODEL", ""), "model name")
		apiKey      = flag.String("api-key", os.Getenv("ABHED_API_KEY"), "API key if required")
		turns       = flag.Int("turns", 12, "conversation turns to simulate")
		prefixKB    = flag.Int("prefix-kb", 24, "approximate size of the stable prefix, in KB")
		jsonOut     = flag.String("json", "", "write the full report to this path")
		testCompact = flag.Bool("compaction", true, "also measure the cold-prefill cost of compaction")
	)
	flag.Parse()

	if *modelName == "" {
		fmt.Fprintln(os.Stderr, "abhed-bench: -model is required (or set ABHED_MODEL)")
		os.Exit(2)
	}

	adapter := model.NewOpenAICompatible(*baseURL, *apiKey, *modelName, model.Profile{Name: *modelName})
	ctx := context.Background()

	rep := report{Endpoint: *baseURL, Model: *modelName}

	// A large, byte-identical prefix is the thing under test: Abhed's system
	// prompt plus memory file plus tool definitions, re-sent every turn.
	prefix := buildPrefix(*prefixKB)
	rep.PrefixTokens = len(prefix) * 10 / 36

	fmt.Printf("abhed-bench\n")
	fmt.Printf("  endpoint  %s\n", *baseURL)
	fmt.Printf("  model     %s\n", *modelName)
	fmt.Printf("  prefix    ~%d tokens (%d KB)\n", rep.PrefixTokens, *prefixKB)
	fmt.Printf("  turns     %d\n\n", *turns)

	var history []model.Message
	fmt.Printf("  %-6s %10s %10s %10s %10s\n", "turn", "prompt", "cached", "cold", "ttft")
	fmt.Printf("  %s\n", strings.Repeat("─", 50))

	for i := 1; i <= *turns; i++ {
		history = append(history, model.Message{
			Role:    model.RoleUser,
			Content: fmt.Sprintf("Turn %d: reply with the single word ACK.", i),
		})

		res, reply, err := measure(ctx, adapter, prefix, history)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nturn %d failed: %v\n", i, err)
			os.Exit(1)
		}
		res.Turn = i
		rep.Turns = append(rep.Turns, res)
		if res.reported {
			rep.CacheReported = true
		}
		history = append(history, model.Message{Role: model.RoleAssistant, Content: reply})

		fmt.Printf("  %-6d %10d %10d %10d %9.0fms\n",
			i, res.PromptTokens, res.CachedTokens, res.ColdTokens, float64(res.TTFT.Microseconds())/1000)
	}

	rep.Summary = summarize(rep.Turns)

	if *testCompact && len(rep.Turns) >= 4 {
		fmt.Printf("\n  measuring compaction penalty...\n")
		cs, err := measureCompaction(ctx, adapter, prefix, history)
		if err == nil {
			rep.Compaction = cs
		} else {
			fmt.Fprintf(os.Stderr, "  compaction measurement failed: %v\n", err)
		}
	}

	printReport(rep)

	if *jsonOut != "" {
		data, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*jsonOut, append(data, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write report: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\nreport written to %s\n", *jsonOut)
	}
}

func measure(ctx context.Context, a model.Adapter, prefix string, history []model.Message) (turnResult, string, error) {
	start := time.Now()
	stream, err := a.Complete(ctx, model.Request{
		System:    prefix,
		Messages:  history,
		MaxTokens: 16,
	})
	if err != nil {
		return turnResult{}, "", err
	}

	var res turnResult
	var reply strings.Builder
	firstToken := time.Time{}

	for chunk := range stream {
		switch chunk.Type {
		case model.ChunkText:
			if firstToken.IsZero() {
				firstToken = time.Now()
			}
			reply.WriteString(chunk.Text)
		case model.ChunkError:
			return turnResult{}, "", chunk.Err
		case model.ChunkDone:
			if chunk.Usage != nil {
				res.PromptTokens = chunk.Usage.InputTokens
				res.CachedTokens = chunk.Usage.CachedInputTokens
				res.reported = chunk.Usage.CacheReported
				res.ColdTokens = res.PromptTokens - res.CachedTokens
			}
		}
	}
	res.Total = time.Since(start)
	if !firstToken.IsZero() {
		res.TTFT = firstToken.Sub(start)
	} else {
		res.TTFT = res.Total
	}
	return res, reply.String(), nil
}

// measureCompaction compares time-to-first-token on a warm prefix against the
// same request after the history is replaced by a summary — the operation that
// invalidates the prefix by construction (docs P8).
func measureCompaction(ctx context.Context, a model.Adapter, prefix string, history []model.Message) (*compactStats, error) {
	warm, _, err := measure(ctx, a, prefix, append(history,
		model.Message{Role: model.RoleUser, Content: "Reply ACK."}))
	if err != nil {
		return nil, err
	}

	// Simulate a compaction: history replaced by a summary. The prefix is
	// unchanged, but everything after it is new.
	compacted := []model.Message{
		{Role: model.RoleUser, Content: "[Earlier conversation compacted. Summary: " +
			strings.Repeat("prior work on the task. ", 40) + "]"},
		{Role: model.RoleUser, Content: "Reply ACK."},
	}
	cold, _, err := measure(ctx, a, prefix, compacted)
	if err != nil {
		return nil, err
	}

	warmMs := float64(warm.TTFT.Microseconds()) / 1000
	coldMs := float64(cold.TTFT.Microseconds()) / 1000
	note := "compaction preserved the system prefix; only the history was re-prefilled"
	if cold.CachedTokens == 0 && warm.CachedTokens > 0 {
		note = "compaction invalidated the entire cached prefix"
	}
	return &compactStats{
		TTFTBeforeMs: warmMs,
		TTFTAfterMs:  coldMs,
		Penalty:      coldMs / math.Max(warmMs, 0.001),
		Note:         note,
	}, nil
}

func summarize(turns []turnResult) summaryStats {
	var s summaryStats
	for _, t := range turns {
		s.TotalPrompt += t.PromptTokens
		s.TotalCached += t.CachedTokens
		s.TotalCold += t.ColdTokens
	}
	if s.TotalPrompt > 0 {
		s.HitRate = float64(s.TotalCached) / float64(s.TotalPrompt)
	}
	if s.TotalCold > 0 {
		s.PrefillFactor = float64(s.TotalPrompt) / float64(s.TotalCold)
	}
	if len(turns) > 0 {
		s.ColdTTFTMs = float64(turns[0].TTFT.Microseconds()) / 1000
	}
	// Median of the warm turns resists a single slow outlier.
	if len(turns) > 2 {
		warm := make([]float64, 0, len(turns)-1)
		for _, t := range turns[1:] {
			warm = append(warm, float64(t.TTFT.Microseconds())/1000)
		}
		sort.Float64s(warm)
		s.WarmTTFTMs = warm[len(warm)/2]
		if s.WarmTTFTMs > 0 {
			s.TTFTSpeedup = s.ColdTTFTMs / s.WarmTTFTMs
		}
	}
	return s
}

// cacheNote says why no cache hit was seen: the endpoint sent no cached-token
// figure, or sent zero on every turn. Empty when some prefix was cached.
func cacheNote(r report) string {
	switch {
	case !r.CacheReported:
		return "did not report a cached-token figure"
	case r.Summary.TotalCached == 0:
		return "reported zero cached tokens on every turn"
	}
	return ""
}

func printReport(r report) {
	s := r.Summary
	fmt.Printf("\n  %s\n", strings.Repeat("═", 50))
	fmt.Printf("  RESULTS\n")
	fmt.Printf("  %s\n", strings.Repeat("═", 50))

	if note := cacheNote(r); note != "" {
		fmt.Printf("\n  ⚠ This endpoint %s.\n\n", note)
		fmt.Printf("    Either prefix caching is disabled, or the server does not\n")
		fmt.Printf("    report prompt_tokens_details.cached_tokens. Abhed's context\n")
		fmt.Printf("    design assumes a working prefix cache — without one, every\n")
		fmt.Printf("    turn pays full prefill and the capacity model in\n")
		fmt.Printf("    docs/architecture/04-sizing.md does not hold for this stack.\n\n")
		fmt.Printf("    For vLLM, enable --enable-prefix-caching.\n")
		fmt.Printf("    TTFT comparison below still indicates whether caching happens.\n")
	}

	fmt.Printf("\n  prompt tokens     %d\n", s.TotalPrompt)
	fmt.Printf("  cached            %d\n", s.TotalCached)
	fmt.Printf("  cold prefill      %d\n", s.TotalCold)
	fmt.Printf("  cache hit rate    %.1f%%\n", s.HitRate*100)
	if s.PrefillFactor > 0 {
		fmt.Printf("  prefill savings   %.1fx  (computed model predicts ~17x for a 40-turn session)\n", s.PrefillFactor)
	}
	fmt.Printf("\n  cold TTFT         %.0f ms   (turn 1)\n", s.ColdTTFTMs)
	fmt.Printf("  warm TTFT         %.0f ms   (median of later turns)\n", s.WarmTTFTMs)
	if s.TTFTSpeedup > 0 {
		fmt.Printf("  TTFT speedup      %.1fx\n", s.TTFTSpeedup)
	}

	if r.Compaction != nil {
		c := r.Compaction
		fmt.Printf("\n  COMPACTION PENALTY\n")
		fmt.Printf("  before compaction %.0f ms\n", c.TTFTBeforeMs)
		fmt.Printf("  after compaction  %.0f ms\n", c.TTFTAfterMs)
		fmt.Printf("  penalty           %.1fx\n", c.Penalty)
		fmt.Printf("  %s\n", c.Note)
	}

	fmt.Printf("\n  VERDICT: ")
	switch {
	case cacheNote(r) != "" && s.TTFTSpeedup < 1.3:
		fmt.Printf("no prefix caching detected.\n")
		fmt.Printf("  Abhed will work, but every turn pays full prefill. Enable prefix\n")
		fmt.Printf("  caching on the serving stack before sizing a deployment.\n")
	case s.HitRate > 0.5 || s.TTFTSpeedup > 1.5:
		fmt.Printf("prefix caching is working.\n")
		fmt.Printf("  The context design in docs/architecture/07-system-prompt.md is\n")
		fmt.Printf("  economical on this stack. Keep the prefix byte-identical across\n")
		fmt.Printf("  turns — one volatile token invalidates all of it.\n")
	default:
		fmt.Printf("caching is partial or inconsistent.\n")
		fmt.Printf("  Check that the system prompt is byte-identical across turns and\n")
		fmt.Printf("  that the server's cache block size suits this prefix length.\n")
	}
}

// buildPrefix approximates Abhed's real stable prefix: system prompt, memory
// file, and tool definitions.
func buildPrefix(kb int) string {
	var b strings.Builder
	b.WriteString("You are Abhed, a software engineering agent operating in a user's codebase.\n\n")
	target := kb * 1024
	section := 0
	for b.Len() < target {
		section++
		fmt.Fprintf(&b, "## Project convention %d\n", section)
		fmt.Fprintf(&b, "- Errors are wrapped with fmt.Errorf and %%w, never returned bare.\n")
		fmt.Fprintf(&b, "- Tests are table-driven; no assertion libraries.\n")
		fmt.Fprintf(&b, "- Package %d owns its own persistence and exposes no I/O helpers.\n\n", section)
	}
	return b.String()
}

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
