package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsoleRenderNoDuplicateReply drives the console's render() headlessly.
//
// The console is JavaScript embedded in a Go string, which makes it the one
// part of Abhed with no test coverage — and it shipped a bug that printed every
// reply twice. The logic is ordinary and testable; only its packaging is
// awkward. So the test extracts render() and its helpers, runs them against a
// minimal DOM in node, and asserts the reply appears exactly once under every
// event ordering the server can produce.
//
// Skipped when node or python3 is unavailable: this must not break a build on a
// machine that has neither.
func TestConsoleRenderNoDuplicateReply(t *testing.T) {
	harness := `import { El } from './dom.mjs';
const tx = new El('div'); tx.id='tx';
globalThis.__root = tx;
const els = { tx };
globalThis.$ = id => els[id] || null;
let turnEl=null, streamEl=null, streamBody=null, live=true, current='s1';
const calls = new Map();
let approvals = new Map();
const stats = {turns:0,tin:0,tout:0,cached:0,tools:{},reason:null,compactions:0};
globalThis.hideThinking = ()=>{};
globalThis.showThinking = ()=>{};
globalThis.refresh = ()=>{};
globalThis.openDrawer = ()=>{};
function newTurn(){ turnEl = node('turn'); tx.appendChild(turnEl); return turnEl; }
function approval(){}
function resolveApproval(){}
`
	if out, err := runConsoleCases(t, "render", harness, "render_cases.mjs"); err != nil {
		t.Fatalf("console render produced a duplicate reply:\n%s", out)
	}
}

// TestConsoleWorkbenchDrawsContentAsText drives the functions that put a file
// and a diff on the page. What they are given is untrusted — a repository's
// files, the agent's edits — and the page runs with the session cookie, so
// none of it may be parsed as markup.
func TestConsoleWorkbenchDrawsContentAsText(t *testing.T) {
	harness := `import { El } from './dom.mjs';
const root = new El('div');
globalThis.__root = root;
const frame = new El('div'); frame.id = 'wb';
const main = new El('div'); main.id = 'wbmain';
root.append(frame); frame.append(main);
const els = { wb: frame, wbmain: main };
globalThis.$ = id => els[id] || null;
`
	if out, err := runConsoleCases(t, "workbench", harness, "workbench_cases.mjs"); err != nil {
		t.Fatalf("the workbench did not draw its content as text:\n%s", out)
	}
}

// The static half of the same rule, which runs where node does not: nothing
// in the workbench's script may assign markup.
func TestConsoleWorkbenchNeverAssignsMarkup(t *testing.T) {
	// From the section's first declaration to the end of its last function.
	start := strings.Index(consoleHTML, "const wb = {")
	last := strings.Index(consoleHTML, "function diffClass(line){")
	if start < 0 || last < start {
		t.Fatal("the workbench section of the console script has moved; update this test")
	}
	end := last + strings.Index(consoleHTML[last:], "\n}\n")
	script := consoleHTML[start:end]
	if !strings.Contains(script, "function showFile(") || !strings.Contains(script, "function loadDir(") {
		t.Fatal("the section found does not hold the workbench's functions")
	}
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(script, sink) {
			t.Errorf("the workbench script uses %s; file content must reach the page as text", sink)
		}
	}
}

// runConsoleCases extracts one set of functions from console.go and runs a
// cases file against them in node, over the minimal DOM in testdata.
func runConsoleCases(t *testing.T, set, harness, casesFile string) (string, error) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping console render test")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed; skipping console render test")
	}

	dir := t.TempDir()
	extracted, err := exec.Command(py, "testdata/extract.py", "console.go", set).Output()
	if err != nil {
		t.Fatalf("extract %s: %v", set, err)
	}
	cases, err := os.ReadFile(filepath.Join("testdata", casesFile))
	if err != nil {
		t.Fatal(err)
	}
	dom, err := os.ReadFile(filepath.Join("testdata", "dom.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dom.mjs"), dom, 0o644); err != nil {
		t.Fatal(err)
	}
	script := harness + string(extracted) + "\n" + string(cases)
	if err := os.WriteFile(filepath.Join(dir, "test.mjs"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(node, "test.mjs")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	t.Log("\n" + strings.TrimSpace(string(out)))
	return string(out), err
}
