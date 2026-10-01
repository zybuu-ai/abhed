# Extracts functions from console.go so the browser logic can be driven
# headlessly. See console_render_test.go.
#
#   extract.py console.go             render() and its helpers
#   extract.py console.go workbench   what draws a file and a diff
#   extract.py console.go conn        the event stream and its reconnect
#   extract.py ide.html ide-md        the workbench's markdown renderer
#   extract.py ide.html ide-render    the workbench's chat render()
#   extract.py ide.html ide-chat      sending, live state and approvals
#   extract.py ide.html ide-conn      the connection indicator
#   extract.py ide.html ide-lines     the line-by-line terminal
#   extract.py ide.html ide-term      the agent's terminal tab
#   extract.py ide.html ide-call      an opened call and the plan
#   extract.py console.go model       the model picker (ide-model: the workbench's)
import pathlib, re, sys
src = pathlib.Path(sys.argv[1]).read_text()
which = sys.argv[2] if len(sys.argv) > 2 else 'render'

def grab(fn):
    i = src.index(fn)
    line_end = src.index('\n', i)
    first = src[i:line_end]
    if first.count('{') == first.count('}') and first.rstrip().rstrip(';').endswith('}'):
        return first
    if fn.startswith('const ') and first.rstrip().endswith(';') and first.count('{') == first.count('}'):
        return first
    j = src.index('\n}\n', i) + 3
    return src[i:j]

sets = {
    'render': ['function node(cls, text){','function md(text){','function lastStreamedBubble(){','function wordCount(s){',
          'function setCollapsed(wrap, on){','function collapse(wrap, on){','function setPeek(wrap, content){',
          'function makeCollapsible(wrap, hdr){','function clip(s, n){','function summarize(tool, args){',
          'function shortPath(p){','function kv(k, v){','function noticeCard(p){','function visible(s, lines){','function reveal(s, lines){','function hasHidden(v, depth = 0){','function offerNext(t){','function takeNext(e){'],
    # render() with the real approval card, for a subagent's ask.
    'ask': ['function node(cls, text){','function md(text){','function lastStreamedBubble(){','function wordCount(s){',
          'function setCollapsed(wrap, on){','function collapse(wrap, on){','function setPeek(wrap, content){',
          'function makeCollapsible(wrap, hdr){','function clip(s, n){','function summarize(tool, args){',
          'function shortPath(p){','function kv(k, v){','function noticeCard(p){','function visible(s, lines){','function reveal(s, lines){','function argsJSON(v){','function hasHidden(v, depth = 0){','function approval(p, rid){','function resolveApproval(callID, outcome, kind, title){','function recheckSoon(){','function offerNext(t){'],
    # The console's event stream and its reconnect.
    'conn': ['function connect(id){'],
    'state': ['function visible(s, lines){','function reveal(s, lines){','function shownState(s){','function paintOpenPill(){','function listBadges(s){'],
    'mode': ['async function loadMode(){'],
    'workbench': ['function node(cls, text){','function visible(s, lines){','function reveal(s, lines){','function fmtSize(n){','function wbShow(name, meta){',
          'function showFile(f){','function viewDiff(f){','function diffClass(line){'],
    # From ide.html: the markdown renderer for replies.
    'ide-md': ['const el = (tag, cls, text) => {','function reveal(s, lines){','function mdInline(parent, s){','function md(text){'],
    # From ide.html: sending, the run's live state and the approval prompt.
    'ide-chat': ['const el = (tag, cls, text) => {','function visible(s, lines){','function reveal(s, lines){','function argsJSON(v){','function hasHidden(v, depth = 0){','function setLive(on){','function offerAsks(){','function forget(b){',
          'function claim(p){','function failed(b, msg, head){','async function unqueue(b){','async function sendNow(b){','async function send(){',
          'function render(ev){','function recheckSoon(){','async function recheck(id){','function askApproval(p, rid){','function focusSoon(){','function settleAsk(callID, how){',
          'function subagentRow(id, p, at){','function drawBg(){','async function watchIdle(){','function offerNext(t){','function takeNext(e){'],
    'ide-conn': ['function setConn(on){','function connLost(retrying){','async function connProbe(){','function connIdle(){','function signInEnded(why){',
          'function connect(id){','async function api(path, opts){','function attach(t, id, reattach){'],
    # From ide.html: the explorer's tree and its name input.
    'ide-tree': ['const el = (tag, cls, text) => {','function reveal(s, lines){','const clear = ','const parentOf = ','const joinPath = ','const dirs = ','const heldLoads = ',
          'function runHeld(){','async function loadTree(path, into, depth){','function refreshDir(path){','function nameInput(anchor, before, depth, initial, done, onEnd){',
          'async function newEntry(folder){'],
    'ide-subject': ['function tail(p){','function subjectOf(tool, args){'],
    'ide-modes': ['function limitModes(m){', 'function modeForNewSession(){'],
    # From ide.html: the Events and HawkEYE panels, which draw record text.
    'ide-panels': ['const el = (tag, cls, text) => {','const clear = ','const fmt = ','function visible(s, lines){','function reveal(s, lines){','function tail(p){','function subjectOf(tool, args){',
          'function logEvent(ev, p){','async function loadHawkeye(){'],
    'ide-render': ['const el = (tag, cls, text) => {','function visible(s, lines){','function reveal(s, lines){','function hasHidden(v, depth = 0){','function render(ev){','function subagentRow(id, p, at){','function drawBg(){','function offerNext(t){'],
    # The model picker, from console.go and from ide.html.
    'model': ['function note(text){','function switchedText(p, was){','function modelLabel(name, model){','function lastNoteText(){','async function loadProviders(){','function chosenProvider(){','function showSessionModel(id){'],
    'ide-model': ['const el = (tag, cls, text) => {','const clear = ','function modelLabel(name, model){','function switchedText(p, was){','function showSwitch(p){','async function loadProviders(){','const modelOf = ','function chosenProvider(){',
          'function showSessionModel(s){','async function switchModel(){'],
    # From ide.html: an opened call's arguments and output, and the plan.
    'ide-call': ['const el = (tag, cls, text) => {','const clear = ','function visible(s, lines){','function reveal(s, lines){','function fillCall(c){','function clip(text){','function drawPlan(items){'],
    # From ide.html: the agent's read-only terminal tab.
    'ide-term': ['function visible(s, lines){','function reveal(s, lines){','function logTerminal(cmd, p, who){'],
    # From ide.html: the line-by-line terminal and its confirmation prompt.
    'ide-lines': ['function visible(s, lines){','function reveal(s, lines){','const linePrompt = ','const promptLine = ','const keySeq = ','function linesData(t, d){','function nextLine(t){',
          'function lineKeys(t, e){','async function completeLine(t){','const unsafeName = ','function wrappedRows(t){','function unclosed(s){','const shellQuote = ','async function runLine(t, cmd, answer){','const CONFIRM_GUARD = ','const askConfirm = ','function confirmData(t, d){'],
}
seen=set(); out=[]
for fn in sets[which]:
    if fn not in src: continue
    name = re.match(r'(?:async function|function|const) (\w+)', fn).group(1)
    if name in seen: continue
    seen.add(name); out.append(grab(fn))
if which in ('render', 'ask'):
    i = src.index('function render(ev){'); j = src.index('function kv(k, v){', i)
    out.append(src[i:j])
sys.stdout.write('\n'.join(out))
