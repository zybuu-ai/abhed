package server

import (
	"strings"
	"testing"
)

// The workbench shows a next prompt from suggestion.offered as the message
// box's placeholder, escaped; Tab puts it in the box and sends nothing; a new
// turn takes it away.
func TestIDEOffersNextPrompt(t *testing.T) {
	harness := `import { El } from './dom.mjs';
globalThis.__root = new El('div');
El.prototype.addEventListener = function(type, f){ (this.on = this.on || {})[type] = f; };
globalThis.__focused = []; El.prototype.focus = function(){ __focused.push(this); };
El.prototype.remove = function(){ const p = this.parentNode; if(p){ p.childNodes.splice(p.childNodes.indexOf(this), 1); this.parentNode = null; } };
Object.defineProperty(El.prototype, 'firstChild', {get(){ return this.childNodes[0] || null; }});
let current = null, live = false, bgLive = false, es = null, lastSeq = 0, endedSeq = 0, recheckTimer = 0, focusTimer = 0;
let streaming = null, streamBody = null, thinkBlock = null, pendThink = '', pendText = '', sessionList = [{id:'s1', prompt:'x'}];
const calls = new Map(), mineCalls = new Set(), queued = new Map(), sent = [], asks = new Map();
const ids = {}, $ = id => ids[id] || (ids[id] = new El('div'));
const tx = () => __root, qbox = new El('div'), add = n => __root.appendChild(n);
let cid = 0; const bubble = (cls, who, text) => { const m = new El('div'); m.className = 'msg ' + cls; m.textContent = text || ''; return m; };
const newCid = () => 'c' + (++cid);
globalThis.__connected = []; let signInGone = false, leaving = false;
const drawQueued = () => {}, withMentions = async s => s, nearBottom = () => true, follow = () => {}, connect = id => { __connected.push(id); };
const waiting = () => {}, flushStream = () => {}, flushSoon = () => {}, endThinking = () => {}, logEvent = () => {};
const loadSessions = () => {}, loadChanges = () => {}, loadHawkeye = () => {}, hawkSoon = () => {}, treeSoon = () => {}, changesSoon = () => {};
const fillCall = () => {}, drawPlan = () => {}, logTerminal = () => {}, subjectOf = (tool, a) => (a && (a.command || a.path)) || '';
const requestAnimationFrame = f => f(), idleTurns = new Map();
// api answers the session list from __sessions, after the next of __delays;
// a route given to __defer answers when the test resolves it.
let __sessions = [], __delays = []; const __pending = {};
globalThis.__defer = route => { let resolve, reject; const p = new Promise((r, j) => { resolve = r; reject = j; }); __pending[route] = p; return {resolve, reject}; };
globalThis.__posted = []; globalThis.__routes = [];
const api = async (url, opts) => {
  const route = ((opts && opts.method) || 'GET') + ' ' + url; __routes.push(route);
  if(opts && opts.body) __posted.push({route, body: JSON.parse(opts.body)});
  if(__pending[route]){ const p = __pending[route]; delete __pending[route]; return p; }
  if(url === '/v1/sessions'){ const snap = __sessions, d = __delays.shift() || 0; if(d) await new Promise(r => setTimeout(r, d)); return snap; }
  const st = /^\/v1\/sessions\/([^/]+)\/state$/.exec(url); if(st){ const s = __sessions.find(x => x.id === st[1]); if(!s) throw Object.assign(new Error('session not found'), {status:404}); return {id:s.id, state:s.state, turns:s.turns}; }
  return [];
};
El.prototype.dispatchEvent = function(e){ (this.dispatched = this.dispatched || []).push(e.type); };
`
	if out, err := runConsoleCases(t, "ide-chat", harness, "ide_suggest_cases.mjs"); err != nil {
		t.Fatalf("the workbench's next prompt failed:\n%s", out)
	}
	for _, want := range []string{"case 'suggestion.offered': if(!live && !$('q').value) offerNext(", "$('q').addEventListener('keydown', takeNext);"} {
		if !strings.Contains(ideHTML, want) {
			t.Errorf("the workbench lost %s", want)
		}
	}
}

// The console does the same with its own message box.
func TestConsoleOffersNextPrompt(t *testing.T) {
	harness := `import { El } from './dom.mjs';
const tx = new El('div'); tx.id='tx';
globalThis.__root = tx;
const q = new El('textarea'); q.placeholder = 'Ask anything';
const els = { tx, q };
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
globalThis.__grown = 0; const autogrow = () => { __grown++; };
function newTurn(){ turnEl = node('turn'); tx.appendChild(turnEl); return turnEl; }
function approval(){}
function resolveApproval(){}
`
	if out, err := runConsoleCases(t, "render", harness, "console_suggest_cases.mjs"); err != nil {
		t.Fatalf("the console's next prompt failed:\n%s", out)
	}
	if !strings.Contains(consoleHTML, "$('q').addEventListener('keydown', takeNext);") {
		t.Error("the console's message box does not take the next prompt on Tab")
	}
}
