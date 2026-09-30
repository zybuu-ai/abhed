package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func TestOutputStyle(t *testing.T) {
	st, store, sf := customRig(t)
	was := managedStyleDir
	managedStyleDir = t.TempDir()
	t.Cleanup(func() { managedStyleDir = was })
	home, _ := os.UserHomeDir()
	write(t, filepath.Join(home, ".abhed", "styles", "terse.md"), "---\ndescription: short answers\n---\nANSWER-TERSELY")
	write(t, filepath.Join(managedStyleDir, "house.md"), "HOUSE-STYLE")
	write(t, filepath.Join(home, ".abhed", "styles", "house.md"), "USER-HOUSE")
	write(t, filepath.Join(st.sess.Root, ".abhed", "styles", "repo.md"), "REPO-STYLE")
	st.loop.Config.SystemPrompt = "BASE"

	typeLine(t, st, "/output-style")
	if out := sf.shown(); !strings.Contains(out, "terse") || !strings.Contains(out, "short answers") || strings.Contains(out, "repo") {
		t.Fatalf("list:\n%s", out)
	}
	typeLine(t, st, "/output-style terse")
	if p := st.loop.Config.SystemPrompt; !strings.HasPrefix(p, "BASE") || !strings.Contains(p, "ANSWER-TERSELY") {
		t.Fatalf("prompt %q", p)
	}
	typeLine(t, st, "/output-style house")
	if p := st.loop.Config.SystemPrompt; strings.Contains(p, "ANSWER-TERSELY") || !strings.Contains(p, "HOUSE-STYLE") || strings.Contains(p, "USER-HOUSE") {
		t.Fatalf("a style was not replaced, or the organisation's lost its name: %q", p)
	}
	typeLine(t, st, "/output-style repo")
	if strings.Contains(st.loop.Config.SystemPrompt, "REPO-STYLE") {
		t.Fatal("a workspace style was used")
	}
	// A new conversation keeps the session's style.
	st.loop.Config.SystemPrompt = "FRESH"
	if err := ensureConversation(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.loop.Config.SystemPrompt, "HOUSE-STYLE") {
		t.Fatal("the style did not follow the session")
	}
	typeLine(t, st, "/output-style off")
	if st.loop.Config.SystemPrompt != "FRESH" {
		t.Fatalf("off left %q", st.loop.Config.SystemPrompt)
	}
	if n := len(storeEventsOf(t, store, agent.EvCommandInvoked)); n != 3 {
		t.Fatalf("%d choices recorded", n)
	}
}

// Memory that happens to contain the style heading loses nothing when a
// style is chosen or changed: managed memory, last in the prompt, survives.
func TestOutputStyleKeepsAPromptThatContainsItsHeading(t *testing.T) {
	st, _, _ := customRig(t)
	home, _ := os.UserHomeDir()
	write(t, filepath.Join(home, ".abhed", "styles", "terse.md"), "ANSWER-TERSELY")
	write(t, filepath.Join(home, ".abhed", "styles", "long.md"), "ANSWER-AT-LENGTH")
	st.loop.Config.SystemPrompt = "BASE\n\n## Output style (terse)\nfrom ABHED.md\n\n## Managed memory\nMANAGED-KEEP"
	typeLine(t, st, "/output-style terse")
	typeLine(t, st, "/output-style long")
	typeLine(t, st, "/output-style off")
	if p := st.loop.Config.SystemPrompt; p != "BASE\n\n## Output style (terse)\nfrom ABHED.md\n\n## Managed memory\nMANAGED-KEEP" {
		t.Fatalf("prompt changed: %q", p)
	}
	if err := ensureConversation(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.loop.Config.SystemPrompt, "MANAGED-KEEP") {
		t.Fatal("applying no style cut the prompt")
	}
}
