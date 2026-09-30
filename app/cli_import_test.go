package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

func TestImportAppendsOnlyWhenConfirmed(t *testing.T) {
	st, store, sf := customRig(t, "", "yes")
	st.loop.Tools = tools.NewRegistry(tools.Write{})
	other := filepath.Join(t.TempDir(), "notes-from-elsewhere.md")
	write(t, other, "use four spaces")
	target := filepath.Join(st.sess.Root, "ABHED.md")

	typeLine(t, st, "/import "+other) // no answer
	if _, err := os.Stat(target); err == nil {
		t.Fatal("imported without a yes")
	}
	typeLine(t, st, "/import "+other)
	data, _ := os.ReadFile(target)
	if !strings.Contains(string(data), "## Imported from notes-from-elsewhere.md\n\nuse four spaces") {
		t.Fatalf("ABHED.md: %q", data)
	}
	if len(sf.asked) != 2 || !strings.Contains(sf.asked[1].Body[0].Text, "use four spaces") {
		t.Fatal("the file was not shown before it was imported")
	}
	if w := eventsOf(t, store, agent.EvMemoryWritten); len(w) != 1 || payloadOf(t, w[0])["kind"] != "import" {
		t.Fatalf("memory.written %+v", w)
	}
}

// Abhed's state and links are never imported.
func TestImportRefusesStateAndLinks(t *testing.T) {
	st, _, sf := customRig(t, "yes", "yes")
	st.loop.Tools = tools.NewRegistry(tools.Write{})
	home, _ := os.UserHomeDir()
	write(t, filepath.Join(home, ".abhed", "secrets.json"), "STATE-CANARY")
	outside := filepath.Join(t.TempDir(), "x.md")
	write(t, outside, "LINK-CANARY")
	if err := os.Symlink(outside, filepath.Join(st.sess.Root, "link.md")); err != nil {
		t.Skip(err)
	}
	typeLine(t, st, "/import ~/.abhed/secrets.json")
	typeLine(t, st, "/import link.md")
	if data, _ := os.ReadFile(filepath.Join(st.sess.Root, "ABHED.md")); len(data) != 0 || len(sf.asked) != 0 {
		t.Fatalf("imported state or a link: %q", data)
	}
}
