package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncatePromptLeavesShortTextUnchanged(t *testing.T) {
	const prompt = "synthetic short prompt"

	if got := truncatePrompt(prompt); got != prompt {
		t.Fatalf("truncatePrompt() = %q, want %q", got, prompt)
	}
}

func TestTruncatePromptUsesByteLimit(t *testing.T) {
	// Synthetic ASCII data makes the 300-byte boundary explicit.
	prompt := strings.Repeat("a", 301)
	want := strings.Repeat("a", 300) + "…"

	if got := truncatePrompt(prompt); got != want {
		t.Fatalf("truncatePrompt() = %q, want %q", got, want)
	}
}

func TestTruncatePromptPreservesRuneBoundaries(t *testing.T) {
	// Synthetic prompt data puts a two-byte rune across the byte limit.
	prompt := strings.Repeat("a", 299) + "é" + strings.Repeat("b", 10)

	got := truncatePrompt(prompt)
	want := strings.Repeat("a", 299) + "…"
	if got != want {
		t.Fatalf("truncatePrompt() = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncatePrompt() returned invalid UTF-8")
	}
}
