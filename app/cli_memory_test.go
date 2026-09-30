package app

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// memory.loaded is recorded once in each conversation, with each file's hash.
func TestMemoryLoadedRecordedOncePerConversation(t *testing.T) {
	st, store, _ := bangRig(t)
	write(t, filepath.Join(st.sess.Root, "ABHED.md"), "use tabs")
	sessionMemory.Store(agent.LoadMemory(agent.MemoryOptions{Workspace: st.sess.Root}))
	t.Cleanup(func() { sessionMemory.Store(nil) })
	recordMemoryLoaded(st)
	recordMemoryLoaded(st)
	ev := eventsOf(t, store, agent.EvMemoryLoaded)
	if len(ev) != 1 {
		t.Fatalf("%d memory.loaded events", len(ev))
	}
	var p agent.MemoryLoaded
	_ = json.Unmarshal(ev[0].Payload, &p)
	if len(p.Files) != 1 || p.Files[0].Path != "ABHED.md" || p.Files[0].Scope != "project" || len(p.Files[0].SHA256) != 64 {
		t.Fatalf("payload %+v", p)
	}
}

// A read deny rule keeps a workspace memory file out of the CLI's memory.
func TestCLIMemoryHonoursDenyRules(t *testing.T) {
	ws := t.TempDir()
	write(t, filepath.Join(ws, "ABHED.md"), "@private/n.md")
	write(t, filepath.Join(ws, "private", "n.md"), "PRIVATE-CANARY")
	pol := policy.New(policy.ModeDefault)
	if err := pol.AddDeny("read(private/**)"); err != nil {
		t.Fatal(err)
	}
	pol.Roots = func() []string { return []string{ws} }
	cfg := config.Default()
	if strings.Contains(agent.LoadMemory(memoryOptions(cfg, pol, ws)).Render(), "PRIVATE-CANARY") {
		t.Fatal("a denied memory import reached the prompt")
	}
	if !strings.Contains(agent.LoadMemory(memoryOptions(cfg, nil, ws)).Render(), "PRIVATE-CANARY") {
		t.Fatal("the import is not read at all")
	}
}

// stubReply answers every request with a short text.
func stubReply(w io.Writer, n int, _ string) {
	fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok %d\"}}]}\n\n", n)
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
}

// stubConfig is a workspace configuration with extra settings and the stub model.
func stubConfig(extra string) func(url string) string {
	return func(url string) string {
		return `{` + extra + `"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + url + `","model":"m","context_window":8192}}}}`
	}
}

// End to end: project memory, its import and a configured rule reach the
// model; imports stop at memory.import_depth; memory.loaded is in the record;
// /memory lists the files by scope.
func TestCLIMemoryHierarchy(t *testing.T) {
	c := startCLIPrepared(t, stubReply, stubConfig(`"rules":{"dirs":["rules"]},"memory":{"import_depth":1},`), func(ws string) {
		write(t, filepath.Join(ws, "AGENTS.md"), "AGENTS-MEMORY @docs/one.md")
		write(t, filepath.Join(ws, "docs", "one.md"), "ONE-IMPORT @two.md")
		write(t, filepath.Join(ws, "docs", "two.md"), "TWO-TOO-DEEP")
		write(t, filepath.Join(ws, "rules", "go.md"), "---\npaths: [\"**/*.go\"]\n---\nGO-RULE")
	})
	c.task("hello")
	c.mu.Lock()
	body := c.bodies[0]
	c.mu.Unlock()
	for _, want := range []string{"AGENTS-MEMORY", "ONE-IMPORT", "GO-RULE", "read because there is no ABHED.md"} {
		if !strings.Contains(body, want) {
			t.Fatalf("%s is not in the prompt", want)
		}
	}
	if strings.Contains(body, "TWO-TOO-DEEP") {
		t.Fatal("an import past memory.import_depth reached the prompt")
	}
	c.command("/memory", "rule go")
	if out := c.out.String(); !strings.Contains(out, "AGENTS.md, read because there is no ABHED.md") || !strings.Contains(out, "memory.import_depth") {
		t.Fatalf("/memory:\n%s", out)
	}
	var loaded []agent.MemoryLoaded
	for _, ev := range c.export() {
		if ev.Type == agent.EvMemoryLoaded {
			var p agent.MemoryLoaded
			_ = json.Unmarshal(ev.Payload, &p)
			loaded = append(loaded, p)
		}
	}
	if len(loaded) != 1 || len(loaded[0].Files) != 3 || loaded[0].Files[0].Path != "AGENTS.md" {
		t.Fatalf("memory.loaded: %+v", loaded)
	}
}
