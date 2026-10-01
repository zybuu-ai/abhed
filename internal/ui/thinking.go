package ui

// Braille frames: one cell wide in every terminal, and they rotate rather
// than flash, which is calmer next to streaming text.
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// The words the activity line walks through while the model works. They are
// about the harness's work, not the model's personality: this is
// infrastructure, and cute verbs age badly on the hundredth run. They change
// every few seconds, so a long wait does not read as a frozen frame.
var thinkingVerbs = []string{
	"Thinking", "Reasoning", "Weighing", "Planning", "Considering",
	"Scoping", "Tracing", "Reading", "Composing", "Deliberating",
	"Working", "Assembling", "Checking", "Resolving", "Drafting",
}

// Verbs is exported for tests that assert the vocabulary stays harness-shaped.
func Verbs() []string { return append([]string(nil), thinkingVerbs...) }
