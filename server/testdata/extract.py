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
#   extract.py ide.html ide-files     downloads, a session's title and delete, notifications
#   extract.py console.go model       the model picker (ide-model: the workbench's)
#   extract.py console.go switch      the chat list, the address and the switcher (ide-switch: the workbench's)
#   extract.py console.go hidden      hasHidden (ide-hidden: the workbench's)
import pathlib, re, sys
src = pathlib.Path(sys.argv[1]).read_text()
which = sys.argv[2] if len(sys.argv) > 2 else 'render'

def grab(fn):
    i = src.index(fn)
    line_end = src.index('\n', i)
    first = src[i:line_end]
    if first.count('{') == first.count('}') and first.rstrip().rstrip(';').endswith('}'):
        return first
    if fn.startswith(('const ', 'let ')) and first.rstrip().endswith(';') and first.count('{') == first.count('}'):
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
    'state': ['function visible(s, lines){','function reveal(s, lines){','function sessionName(s){','function shownState(s){','function paintOpenPill(){','function listBadges(s){'],
    'mode': ['async function loadMode(){'],
    # The console's chat list: order, search, the address, the switcher and rename.
    'switch': ['function visible(s, lines){','function reveal(s, lines){','function sessionName(s){','function shownState(s){','function listBadges(s){','function ago(iso){','function dayGroup(iso){',
          'const sessionTime = ','function sortSessions(list){','function matchSession(s, q){','const SESSION_ID = ','function sessionFromURL(){','function setURL(id){','async function restoreFromURL(){',
          'function headTitle(s){','function renameInPlace(span, s){','async function setTitle(s, title){','function neighbour(dir){','let qsHits = ','function drawSwitcher(){','function switchKeys(e){',
          'function sessionRow(s){','async function refresh(){'],
    'workbench': ['function node(cls, text){','function visible(s, lines){','function reveal(s, lines){','function fmtSize(n){','function wbShow(name, meta){',
          'function showFile(f){','function viewDiff(f){','function diffClass(line){'],
    # From ide.html: the markdown renderer for replies.
    'ide-md': ['const el = (tag, cls, text) => {','function reveal(s, lines){','function mdInline(parent, s){','function md(text){'],
    # From ide.html: sending, the run's live state and the approval prompt.
    'ide-chat': ['const el = (tag, cls, text) => {','const clear = ','function visible(s, lines){','function reveal(s, lines){','function argsJSON(v){','function hasHidden(v, depth = 0){','function setLive(on){','function offerAsks(){','function forget(b){',
          'function userBubble(text, state){','function claim(p){','function failed(b, msg, head){','async function unqueue(b){','async function sendNow(b){','async function send(){',
          'function render(ev){','function recheckSoon(){','async function recheck(id){','function askApproval(p, rid){','function focusSoon(){','function settleAsk(callID, how){',
          'function subagentRow(id, p, at){','function drawStop(){','function endBg(){','function drawBg(){','async function watchIdle(){','function offerNext(t){','function takeNext(e){',
          'const MAX_UPLOAD = ','let pending = ','function addFiles(list){','function drawFiles(){','let reviewNotes = ','function addNote(path, line, code, text){','function drawNotes(){','function withNotes(text, notes){','async function postFile(id, file, name, dir){','async function flushUploads(id){','function withFiles(prompt, uploaded){',
          'const NOTE_TEXT = ','const notifyOn = ','function notify(kind){','let lastUser = ','const EDIT_HINT = ','function offerEdit(){','function editLast(b){','function stopEditing(){'],
    'ide-conn': ['function setConn(on){','function connLost(retrying){','async function connProbe(){','function connIdle(){','function signInEnded(why){',
          'function connect(id){','function streamLost(id){','const STARTING_WAITS = ','function stillStarting(id, what, again){','async function notHere(id){',
          'async function api(path, opts){','function attach(t, id, reattach){'],
    # From ide.html: the explorer's tree and its name input.
    'ide-tree': ['const el = (tag, cls, text) => {','function reveal(s, lines){','const clear = ','const parentOf = ','const joinPath = ','const dirs = ','const heldLoads = ',
          'function runHeld(){','async function loadTree(path, into, depth){','function refreshDir(path){','function nameInput(anchor, before, depth, initial, done, onEnd, hint){',
          'async function newEntry(folder){','const hasFiles = ','function dropTarget(node, dir){','const MAX_UPLOAD = ','async function postFile(id, file, name, dir){','async function uploadToFolder(dir, files){'],
    # From ide.html: downloads, and a session's title, delete and resume command.
    'ide-files': ['const el = (tag, cls, text) => {','const clear = ','function visible(s, lines){','function reveal(s, lines){','const fmtSize = ','async function download(path, report){','async function loadDownloads(){',
          'const sessionLabel = ','function editTitle(span, s){','async function setTitle(s, title, span){','async function deleteSession(s){','const resumeCommand = ','async function copyResume(id, btn){','function exportSession(id, format){',
          'const NOTE_TEXT = ','const notifyOn = ','function notify(kind){','function drawNotify(){','async function toggleNotify(){'],
    # hasHidden alone, from either page: ide-hidden takes ide.html's.
    'hidden': ['function visible(s, lines){','function reveal(s, lines){','function hasHidden(v, depth = 0){'],
    'ide-hidden': ['function visible(s, lines){','function reveal(s, lines){','function hasHidden(v, depth = 0){'],
    # From ide.html: the background task list behind the status bar and /tasks.
    'ide-tasks': ['const el = (tag, cls, text) => {','const clear = ','function visible(s, lines){','function reveal(s, lines){','async function showTasks(anchor){'],
    # From ide.html: leaving a session for a new one.
    'ide-reset': ['function visible(s, lines){','function reveal(s, lines){','function resetSession(){','function offerNext(t){'],
    # The explorer's tree with its rename.
    'ide-rename': ['const el = (tag, cls, text) => {','function visible(s, lines){','function reveal(s, lines){','const clear = ','const parentOf = ','const joinPath = ','const dirs = ','const heldLoads = ',
          'function runHeld(){','async function loadTree(path, into, depth){','function refreshDir(path){','function nameInput(anchor, before, depth, initial, done, onEnd, hint){',
          'const hasFiles = ','function dropTarget(node, dir){','const unsafeName = ','function renameEntry(e, row){'],
    # From ide.html: the @ list and the command palette, which draw workspace file names.
    'ide-at': ['const el = (tag, cls, text) => {','const clear = ','function visible(s, lines){','function reveal(s, lines){','function drawAt(){'],
    'ide-pal': ['const el = (tag, cls, text) => {','const clear = ','function visible(s, lines){','function reveal(s, lines){','const sessionLabel = ','function palItems(){','function drawPal(){'],
    'ide-subject': ['function tail(p){','function subjectOf(tool, args){'],
    # From ide.html: the session list's order and filter, the address, the dropdown and its keys.
    'ide-switch': ['const el = (tag, cls, text) => {','const clear = ','function visible(s, lines){','function reveal(s, lines){','const sessionLabel = ',
          'const sessionTime = ','function sortSessions(list){','function matchSession(s, q){','function ageOf(iso){','const dayOf = ','function drawSessions(){',
          'const SESSION_ID = ','function sessionFromURL(){','function setURL(id){','let startNote = ','async function startingSession(){','function neighbour(dir){',
          'let dropSel = ','function drawDrop(){','function switchKeys(e){'],
    'ide-modes': ['function limitModes(m){', 'function modeForNewSession(){'],
    # From ide.html: the Events and HawkEYE panels, which draw record text.
    'ide-panels': ['const el = (tag, cls, text) => {','const clear = ','const fmt = ','function visible(s, lines){','function reveal(s, lines){','function tail(p){','function subjectOf(tool, args){',
          'function logEvent(ev, p){','async function loadHawkeye(){'],
    'ide-render': ['const el = (tag, cls, text) => {','function visible(s, lines){','function reveal(s, lines){','function hasHidden(v, depth = 0){','function bubble(cls, who, text){','function render(ev){','function subagentRow(id, p, at){','function drawStop(){','function endBg(){','function drawBg(){','function offerNext(t){','let lastUser = ','function offerEdit(){'],
    # The model picker, from console.go and from ide.html.
    'model': ['function visible(s, lines){','function reveal(s, lines){','function note(text){','function switchedText(p, was){','function modelLabel(name, model){','function lastNoteText(){','async function loadProviders(){','function chosenProvider(){','function showSessionModel(id){'],
    'ide-model': ['const el = (tag, cls, text) => {','const clear = ','function visible(s, lines){','function reveal(s, lines){','function modelLabel(name, model){','function switchedText(p, was){','function showSwitch(p){','async function loadProviders(){','const modelOf = ','function chosenProvider(){',
          'function showSessionModel(s){','async function switchModel(){'],
    # From ide.html: an opened call's arguments and output, and the plan.
    'ide-call': ['const el = (tag, cls, text) => {','const clear = ','function visible(s, lines){','function reveal(s, lines){','function fillCall(c){','function clip(text){','function drawPlan(items){'],
    # From ide.html: the agent's read-only terminal tab.
    'ide-term': ['function visible(s, lines){','function reveal(s, lines){','function logTerminal(cmd, p, who){'],
    # From ide.html: the line-by-line terminal and its confirmation prompt.
    'ide-lines': ['function visible(s, lines){','function reveal(s, lines){','const linePrompt = ','const promptLine = ','const keySeq = ','function linesData(t, d){','function nextLine(t){',
          'function lineKeys(t, e){','async function completeLine(t){','const unsafeName = ','const hiddenChar = ','const echoed = ','function wrappedRows(t){','function unclosed(s){','const shellQuote = ','async function runLine(t, cmd, answer){','const CONFIRM_GUARD = ','const askConfirm = ','function confirmData(t, d){'],
}
seen=set(); out=[]
for fn in sets[which]:
    if fn not in src: continue
    name = re.match(r'(?:async function|function|const|let) (\w+)', fn).group(1)
    if name in seen: continue
    seen.add(name); out.append(grab(fn))
if which in ('render', 'ask'):
    i = src.index('function render(ev){'); j = src.index('function kv(k, v){', i)
    out.append(src[i:j])
sys.stdout.write('\n'.join(out))
