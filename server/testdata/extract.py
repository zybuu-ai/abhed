# Extracts functions from console.go so the browser logic can be driven
# headlessly. See console_render_test.go.
#
#   extract.py console.go             render() and its helpers
#   extract.py console.go workbench   what draws a file and a diff
#   extract.py ide.html ide-md        the workbench's markdown renderer
#   extract.py ide.html ide-render    the workbench's chat render()
#   extract.py ide.html ide-chat      sending, live state and approvals
#   extract.py ide.html ide-conn      the connection indicator
#   extract.py ide.html ide-lines     the line-by-line terminal
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
          'function shortPath(p){','function kv(k, v){'],
    'workbench': ['function node(cls, text){','function fmtSize(n){','function wbShow(name, meta){',
          'function showFile(f){','function viewDiff(f){','function diffClass(line){'],
    # From ide.html: the markdown renderer for replies.
    'ide-md': ['const el = (tag, cls, text) => {','function mdInline(parent, s){','function md(text){'],
    # From ide.html: sending, the run's live state and the approval prompt.
    'ide-chat': ['const el = (tag, cls, text) => {','function setLive(on){','function offerAsks(){','function forget(b){',
          'function claim(p){','function failed(b, msg){','async function unqueue(b){','async function sendNow(b){','async function send(){',
          'function render(ev){','function recheckSoon(){','async function recheck(id){','function askApproval(p, rid){','function focusSoon(){','function settleAsk(callID, how){'],
    'ide-conn': ['function setConn(on){','function connLost(retrying){','async function connProbe(){','function signInEnded(){',
          'function connect(id){','async function api(path, opts){','function attach(t, id, reattach){'],
    'ide-render': ['const el = (tag, cls, text) => {','function render(ev){'],
    # From ide.html: the line-by-line terminal and its confirmation prompt.
    'ide-lines': ['const linePrompt = ','const promptLine = ','const keySeq = ','function linesData(t, d){','function nextLine(t){',
          'function lineKeys(t, e){','async function completeLine(t){','const unsafeName = ','function wrappedRows(t){','function unclosed(s){','const shellQuote = ','async function runLine(t, cmd, answer){','function confirmData(t, d){','const isPlainCd = '],
}
seen=set(); out=[]
for fn in sets[which]:
    if fn not in src: continue
    name = re.match(r'(?:async function|function|const) (\w+)', fn).group(1)
    if name in seen: continue
    seen.add(name); out.append(grab(fn))
if which == 'render':
    i = src.index('function render(ev){'); j = src.index('function kv(k, v){', i)
    out.append(src[i:j])
sys.stdout.write('\n'.join(out))
