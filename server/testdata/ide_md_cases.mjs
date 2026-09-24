// Assertions driven against the workbench's real markdown renderer, spliced in
// above this file's contents by ide_test.go.
const HOSTILE = '<img src=x onerror=alert(1)><script>alert(2)</script>';

let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const all = (n, out = []) => { for(const c of n.childNodes || []){ if(c.nodeType === 1){ out.push(c); all(c, out); } } return out; };
const tags = n => all(n).map(c => c.tag);
const markupSet = n => n.nodeType === 1 && (Boolean(n._html) || n.childNodes.some(markupSet));

let root = md(HOSTILE + '\n**bold ' + HOSTILE + '** `' + HOSTILE + '`\n[click](javascript:alert(1)) [site](https://example.com) [x](data:text/html,hi)');
check('markup in a reply stays text', root.textContent.includes(HOSTILE) && !tags(root).some(t => t === 'img' || t === 'script') && !markupSet(root));
const links = all(root).filter(n => n.tag === 'a');
check('only http, https and mailto become links', links.length === 1 && links[0].href === 'https://example.com');
check('a link opens apart from the page', links[0].target === '_blank' && links[0].rel === 'noopener noreferrer');
check('an unsafe link is left as its text', root.textContent.includes('[click](javascript:alert(1))'));
check('bold and code are elements', tags(root).includes('strong') && tags(root).includes('code'));

root = md('# Title\n\n- a\n- b\n\n1. one\n2. two\n\n```go\nfmt.Println("<x>")\n```\n\n| h1 | h2 |\n|---|---|\n| c1 | c2 |\n\n> quoted\n\nplain *em* and `code`\nnext line');
const t = tags(root);
check('a heading is drawn below the bubble title size', t.includes('h3'));
check('lists keep their kind and items', t.filter(x => x === 'ul').length === 1 && t.filter(x => x === 'ol').length === 1 && t.filter(x => x === 'li').length === 4);
const pre = all(root).find(n => n.tag === 'pre');
check('a fence keeps its text verbatim', pre && pre.textContent === 'fmt.Println("<x>")' && pre.dataset.lang === 'go');
check('a table has its header and cells', t.filter(x => x === 'th').length === 2 && t.filter(x => x === 'td').length === 2);
check('a quote, emphasis and a soft break', t.includes('blockquote') && t.includes('em') && t.includes('br'));

root = md('```\nno end ' + HOSTILE);
check('an unclosed fence runs to the end, as text', root.textContent === 'no end ' + HOSTILE && !markupSet(root));

process.exit(ok ? 0 : 1);
