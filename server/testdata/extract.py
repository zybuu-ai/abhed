# Extracts functions from console.go so the browser logic can be driven
# headlessly. See console_render_test.go.
#
#   extract.py console.go             render() and its helpers
#   extract.py console.go workbench   what draws a file and a diff
import pathlib, re, sys
src = pathlib.Path(sys.argv[1]).read_text()
which = sys.argv[2] if len(sys.argv) > 2 else 'render'

def grab(fn):
    i = src.index(fn)
    line_end = src.index('\n', i)
    first = src[i:line_end]
    if first.count('{') == first.count('}') and first.rstrip().endswith('}'):
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
}
seen=set(); out=[]
for fn in sets[which]:
    if fn not in src: continue
    name = re.match(r'function (\w+)', fn).group(1)
    if name in seen: continue
    seen.add(name); out.append(grab(fn))
if which == 'render':
    i = src.index('function render(ev){'); j = src.index('function kv(k, v){', i)
    out.append(src[i:j])
sys.stdout.write('\n'.join(out))
