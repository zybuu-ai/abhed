package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
let turnEl=null, streamEl=null, streamBody=null, live=true, current='s1', bgLive=false, es=null;
globalThis.__connected = []; const connect = id => { __connected.push(id); };
const calls = new Map();
let approvals = new Map();
const stats = {turns:0,tin:0,tout:0,cached:0,tools:{},reason:null,compactions:0};
globalThis.hideThinking = ()=>{};
globalThis.showThinking = ()=>{};
globalThis.refresh = ()=>{};
globalThis.openDrawer = ()=>{};
globalThis.paintOpenPill = ()=>{};
function newTurn(){ turnEl = node('turn'); tx.appendChild(turnEl); return turnEl; }
function approval(){}
function resolveApproval(){}
`
	if out, err := runConsoleCases(t, "render", harness, "render_cases.mjs"); err != nil {
		t.Fatalf("console render produced a duplicate reply:\n%s", out)
	}
}

// A subagent's ask is drawn as an approval card that says whose it is and
// that Always allow covers the session, answered by the subagent's request id
// on the parent session, and settled by the subagent.action that follows.
func TestConsoleAsksForASubagent(t *testing.T) {
	harness := `import { El } from './dom.mjs';
El.prototype.remove = function(){ const p = this.parentNode; if(p){ p.childNodes.splice(p.childNodes.indexOf(this), 1); this.parentNode = null; } };
const tx = new El('div'); tx.id='tx';
globalThis.__root = tx;
const els = { tx, stop: new El('button') };
globalThis.$ = id => els[id] || null;
let turnEl=null, streamEl=null, streamBody=null, live=true, current='s1', bgLive=false, es=null;
globalThis.__connected = []; const connect = id => { __connected.push(id); };
const calls = new Map();
let approvals = new Map();
const stats = {turns:0,tin:0,tout:0,cached:0,tools:{},reason:null,compactions:0};
globalThis.hideThinking = ()=>{};
globalThis.showThinking = ()=>{};
globalThis.refresh = ()=>{};
globalThis.openDrawer = ()=>{};
globalThis.paintOpenPill = ()=>{};
globalThis.__posted = [];
let __api = async (path, opts) => { __posted.push({path, body: JSON.parse(opts.body)}); return null; };
const api = (path, opts) => __api(path, opts);
function newTurn(){ turnEl = node('turn'); tx.appendChild(turnEl); return turnEl; }
`
	if out, err := runConsoleCases(t, "ask", harness, "console_ask_cases.mjs"); err != nil {
		t.Fatalf("the console's subagent ask failed:\n%s", out)
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

// The console lists a finished session by how it ended, and the open one by
// its record's end before the list has caught up.
func TestConsoleListNamesHowASessionEnded(t *testing.T) {
	harness := `import { El } from './dom.mjs';
let current = null, live = false;
const stats = {reason:null};
const els = { list: new El('div') };
globalThis.$ = id => els[id] || null;
`
	if out, err := runConsoleCases(t, "state", harness, "list_state_cases.mjs"); err != nil {
		t.Fatalf("the console's session states failed:\n%s", out)
	}
}

// The console's mode selector starts on the server's configured mode, which
// with plan is all a session may start in: it once loaded on default, which a
// server configured otherwise refuses.
func TestConsoleModeFollowsTheServer(t *testing.T) {
	harness := `import { El } from './dom.mjs';
let sel;
globalThis.reset = () => {
  sel = {value: 'default', options: [], appendChild(o){ this.options.push(o); return o; }};
  for(const v of ['default', 'plan', 'accept-edits', 'auto']) sel.options.push({value: v, textContent: v, disabled: false});
};
reset();
globalThis.__caps = null;
const $ = id => id === 'mode' ? sel : null;
const api = async path => { if(path !== '/v1/capabilities' || !__caps) throw new Error('no'); return __caps; };
`
	if out, err := runConsoleCases(t, "mode", harness, "mode_cases.mjs"); err != nil {
		t.Fatalf("the console's mode selector failed:\n%s", out)
	}
	if !strings.Contains(consoleHTML, "loadProviders(); loadMode();") {
		t.Error("the console does not load the server's mode when it starts")
	}
}

// The rail's pill is repainted when the open session's end renders, not only
// on the next list poll, which a drain never lets succeed.
func TestConsoleEndRepaintsTheOpenPill(t *testing.T) {
	i := strings.Index(consoleHTML, "case 'session.ended': {")
	if i < 0 {
		t.Fatal("the console no longer renders session.ended")
	}
	end := i + strings.Index(consoleHTML[i:], "break;")
	body := consoleHTML[i:end]
	if r, p := strings.Index(body, "stats.reason = p.reason;"), strings.Index(body, "paintOpenPill();"); r < 0 || p < r {
		t.Fatal("session.ended does not repaint the open session's pill after recording its end")
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
	// CI sets ABHED_REQUIRE_PAGE_TESTS, so a missing tool fails there rather than skipping.
	skip := t.Skip
	if os.Getenv("ABHED_REQUIRE_PAGE_TESTS") != "" {
		skip = t.Fatal
	}
	node, err := exec.LookPath("node")
	if err != nil {
		skip("node not installed; skipping console render test")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		skip("python3 not installed; skipping console render test")
	}

	dir := t.TempDir()
	src := "console.go"
	if strings.HasPrefix(set, "ide") {
		src = "ide.html"
	}
	extracted, err := exec.Command(py, "testdata/extract.py", src, set).Output()
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

// The model picker is the console's only way to switch a session's model. A
// width rule that hides it removes the control, so it must always have a
// place: the header on wider screens, the rail on a phone.
func TestConsoleKeepsTheModelPickerReachable(t *testing.T) {
	for _, want := range []string{`id="mdlstat"`, `id="railmodel"`, "function placeModel()", "matchMedia('(max-width:760px)')",
		"if(sw && !sw.hidden) $('railmodel').appendChild(sw)", ".rail-model #switchuser:not([hidden])"} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("the console lost %s", want)
		}
	}
	i := strings.Index(consoleHTML, "@media (min-width:761px) and (max-width:1180px){\n  .top .brand")
	if i < 0 {
		t.Fatal("the tablet header rule moved; check the model picker is still shown there")
	}
	rule := consoleHTML[i : i+strings.Index(consoleHTML[i:], "}")]
	for _, hidden := range []string{"#mdlstat", "#mdlpick", ".stat:not", ".top .stat{", "#switchuser"} {
		if strings.Contains(rule, hidden) {
			t.Errorf("the tablet header rule hides %s, a control that must stay reachable", hidden)
		}
	}
	// The workbench keeps Switch at every width; the Enterprise guide promises it.
	if m := regexp.MustCompile(`(?m)^.*#switchuser[^{]*\{[^}]*display:none`).FindString(ideHTML); m != "" {
		t.Errorf("the workbench hides Switch: %s", m)
	}
}

// On a phone the workbench's composer row must keep Send on screen: the
// selects and the hint shrink, Send never does.
func TestIDEComposerRowKeepsSendOnScreen(t *testing.T) {
	for _, want := range []string{
		"#crow select{font:12px var(--mono);background:var(--bg);color:var(--ink);border:1px solid var(--line-strong);border-radius:5px;padding:3px 6px;min-width:0;flex:0 1 auto}",
		"#crow .hint{flex:1 1 0;min-width:0;", "#send{flex:none;",
	} {
		if !strings.Contains(ideHTML, want) {
			t.Errorf("the composer row lost %s, which keeps Send on screen at 360 px", want)
		}
	}
}

// pickerHarness is the page state the model pickers run against: a select
// that holds its chosen option, two providers, and an api that records posts.
const pickerHarness = `import { El } from './dom.mjs';
globalThis.__root = new El('div');
class Sel extends El {
  appendChild(o){ if(o.selected || this._v === undefined) this._v = o.value; return super.appendChild(o); }
  get value(){ return this._v; } set value(v){ this._v = v; }
}
const els = { mdlpick: new Sel('select'), tx: __root };
els.mdlpick.hidden = true;
const $ = id => els[id] || (els[id] = new El('div'));
let current = null, providers = [], sessionsSeen = [], sessionList = [], caps = null, recProvider = null;
Object.defineProperty(El.prototype, 'lastElementChild', {get(){ return this.childNodes[this.childNodes.length - 1] || null; }});
const add = n => __root.appendChild(n);
El.prototype.removeChild = function(c){ this.childNodes.splice(this.childNodes.indexOf(c), 1); return c; };
Object.defineProperty(El.prototype, 'firstChild', {get(){ return this.childNodes[0] || null; }});
globalThis.__posted = []; let __fail = null;
const api = async (url, opts) => {
  if(url === '/v1/providers') return globalThis.__providers || [{name:'a', model:'model-a', default:true}, {name:'b', model:'model-b', default:false}];
  __posted.push({url, body: opts && opts.body ? JSON.parse(opts.body) : null});
  if(__fail) throw new Error(__fail);
  return globalThis.__reply || {provider:'b', model:'model-b', from:'model-a'};
};
`

// The console's picker sends a new chat's choice, follows the open chat, and
// on a refused switch says why and shows the model still in use.
func TestConsoleModelPicker(t *testing.T) {
	if out, err := runConsoleCases(t, "model", pickerHarness, "model_cases.mjs"); err != nil {
		t.Fatalf("the console's model picker failed:\n%s", out)
	}
	// Static: a new chat carries the picker's choice.
	if !strings.Contains(consoleHTML, "mode: $('mode').value, provider: chosenProvider()})") {
		t.Error("the console starts a chat without the model the picker shows")
	}
}

// The workbench has the same picker, for a new session and an open one.
func TestIDEModelPicker(t *testing.T) {
	if out, err := runConsoleCases(t, "ide-model", pickerHarness, "ide_model_cases.mjs"); err != nil {
		t.Fatalf("the workbench's model picker failed:\n%s", out)
	}
	for _, want := range []string{"JSON.stringify({workbench:true, mode, provider})", "client_id: b.cid, provider})"} {
		if !strings.Contains(ideHTML, want) {
			t.Errorf("the workbench starts a session without the model the picker shows: no %s", want)
		}
	}
}

// The rail's rows carry the badges: background tasks and a waiting approval,
// in the console and in the workbench.
func TestListRowsShowBackgroundBadges(t *testing.T) {
	if !strings.Contains(consoleHTML, "m.append(pill, ...listBadges(s), when)") {
		t.Fatal("the console's list rows do not show the background and approval badges")
	}
	if !strings.Contains(ideHTML, "if(s.background) row.appendChild(") || !strings.Contains(ideHTML, "if(s.pending_ask) row.appendChild(") {
		t.Fatal("the workbench's list rows do not show the background and approval badges")
	}
}

// The console reopens a dropped event stream while a run is live or
// background work is owed, and not for a finished session.
func TestConsoleReconnectsWhileBackgroundOwed(t *testing.T) {
	harness := `
globalThis.__streams = [];
globalThis.EventSource = class { constructor(url){ this.url = url; __streams.push(this); } close(){} };
globalThis.setTimeout = f => f();
let es = null, lastSeq = 0, live = false, bgLive = false, current = null;
const render = () => {}, workbenchSaw = () => {};
`
	if out, err := runConsoleCases(t, "conn", harness, "console_conn_cases.mjs"); err != nil {
		t.Fatalf("the console's reconnect failed:\n%s", out)
	}
}
