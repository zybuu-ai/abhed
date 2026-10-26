package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cardHarness fakes what ollama_card.js reaches: the page, fetch and confirm.
const cardHarness = `import { El } from './dom.mjs';
globalThis.__root = new El('div');
document.head = new El('head');
El.prototype.addEventListener = function(type, fn){ this['on' + type] = fn; };
El.prototype.remove = function(){ const p = this.parentNode; if(p){ p.childNodes.splice(p.childNodes.indexOf(this), 1); this.parentNode = null; } };
globalThis.__posted = []; globalThis.__asked = []; globalThis.__confirm = false; globalThis.__replies = {}; globalThis.__stream = [];
globalThis.confirm = q => { __asked.push(q); return __confirm; };
globalThis.__fetched = [];
globalThis.fetch = async (url, opts) => {
  __fetched.push(url);
  const body = opts && opts.body ? JSON.parse(opts.body) : null;
  if(opts && opts.method === 'POST') __posted.push({url, body});
  if(url === '/v1/ollama/pull'){
    const lines = __stream.map(l => l + '\n'); let i = 0;
    return {ok: true, status: 200, body: {getReader: () => ({read: async () => i < lines.length ? {done: false, value: new TextEncoder().encode(lines[i++])} : {done: true}})}};
  }
  const list = __replies[url] || [];
  const v = list.length > 1 ? list.shift() : list[0];
  return {ok: !!v, status: v ? 200 : 404, json: async () => v || {error: 'no reply'}};
};
`

// The Ollama card asks before every action, shows the exact command, the
// size and the steps as text, and goes once the model is ready.
func TestOllamaCard(t *testing.T) {
	skip := t.Skip
	if os.Getenv("ABHED_REQUIRE_PAGE_TESTS") != "" {
		skip = t.Fatal
	}
	node, err := exec.LookPath("node")
	if err != nil {
		skip("node not installed; skipping the card test")
	}
	for _, page := range []string{idePage, consoleHTML} {
		if !strings.Contains(page, "const abhedOllama = ") || strings.Contains(page, "/*OLLAMA-CARD*/") {
			t.Fatal("a page lacks the Ollama card")
		}
	}
	if strings.Contains(ollamaCardJS, "innerHTML") {
		t.Fatal("the card writes markup")
	}
	dir := t.TempDir()
	for _, f := range []string{"dom.mjs", "ollama_card_cases.mjs"} {
		b, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases, _ := os.ReadFile(filepath.Join(dir, "ollama_card_cases.mjs"))
	script := cardHarness + ollamaCardJS + "\n" + string(cases)
	if err := os.WriteFile(filepath.Join(dir, "test.mjs"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "test.mjs")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	t.Log("\n" + strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("the card failed: %v", err)
	}
}
