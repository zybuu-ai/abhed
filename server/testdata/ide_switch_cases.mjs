// The workbench's session list, the address, the dropdown and its keys,
// driven against the real functions spliced in above by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const at = q => { location.search = q; location.href = 'https://h/ide' + q; };
const NBSP = String.fromCharCode(0xa0);
const now = Date.now(), iso = ms => new Date(now - ms).toISOString();
sessionList = sortSessions([
  {id:'s-a', prompt:'fix the retry loop', created:iso(9e6), updated:iso(60e3), state:'done'},
  {id:'s-b', prompt:'terraform cleanup', title:'Zebra' + NBSP + 'renamed', created:iso(8e6), updated:iso(3 * 86400e3), state:'done'},
  {id:'s-c', prompt:'release notes draft', created:iso(90e3), state:'idle'},
]);
check('the list is in order of last activity', sessionList.map(s => s.id).join() === 's-a,s-c,s-b');

// The Sessions view.
current = 's-c';
drawSessions();
const rows = $('sessions').childNodes.filter(n => n.tag === 'button');
check('every session is listed', rows.length === 3);
check('with its age', rows[0].textContent.endsWith('1m') && rows[2].textContent.endsWith('3d'));
check('under Today and Earlier', $('sessions').childNodes.filter(n => n.className === 'sgrp').map(n => n.textContent).join() === 'Today,Earlier');
check('the open one is marked', rows[1].attrs['aria-current'] === 'true' && rows[0].attrs['aria-current'] === 'false');
check('a title keeps its hidden characters written out', rows[2].textContent.includes('Zebra⟨U+00A0⟩renamed'));
$('sfilter').value = 'zebra';
drawSessions();
check('the filter matches a title', $('sessions').childNodes.filter(n => n.tag === 'button').length === 1);
$('sfilter').value = 'nothing like it';
drawSessions();
check('and says when nothing matches', $('sessions').textContent.startsWith('No sessions match'));
$('sfilter').value = '';

// The dropdown.
current = 's-a'; dropSel = 0; dropHits = [];
$('sdropq').value = '';
drawDrop();
let opts = $('sdropl').childNodes;
check('the dropdown lists every session', opts.length === 3);
check('the current one is marked', opts[0].textContent.includes('● current') && opts[0].attrs['aria-current'] === 'true');
check('and the selection starts on the next', opts[1].attrs['aria-selected'] === 'true');
$('sdropq').value = 'terraform'; dropSel = 0; dropHits = [];
drawDrop();
check('it filters by the first message', $('sdropl').childNodes.length === 1 && $('sdropl').textContent.includes('Zebra'));
$('sdropq').value = 'nothing like it'; drawDrop();
check('and says when nothing matches', $('sdropl').textContent === 'No sessions match.');

// The address and the tab.
at('');
setURL('s-c');
check('opening a session puts it in the address', location.search === '?s=s-c' && __tab['abhed.ide.session'] === 's-c');
setURL(null);
check('a new session leaves it', location.search === '' && !('abhed.ide.session' in __tab));
at('?s=s-b');
check('a reload opens the session in the address, not the latest', (await startingSession()).id === 's-b');
at('?s=s-older'); __states['s-older'] = {state:'done'};
let first = await startingSession();
check('one beyond the list is asked for by id', first.id === 's-older' && first.state === 'done');
at(''); __tab['abhed.ide.session'] = 's-c';
check('with no address, this tab\'s last session', (await startingSession()).id === 's-c');
delete __tab['abhed.ide.session'];
check('and with neither, the latest active', (await startingSession()).id === 's-a');
at('?s=s-gone'); __states['s-gone'] = null; startNote = '';
check('one that is gone falls back to the latest', (await startingSession()).id === 's-a' && startNote.includes('not one you can open'));
at('?s=..%2Fx');
check('an address that is not an id is ignored', sessionFromURL() === null);

// Previous, next and the keys.
current = 's-c';
check('previous and next follow the list', neighbour(-1).id === 's-a' && neighbour(1).id === 's-b');
const key = (k, extra = {}) => ({key:k, code:extra.code || '', metaKey:false, ctrlKey:false, altKey:false, shiftKey:false, target:{tagName:'DIV', closest:() => null}, preventDefault(){ this.prevented = true; }, ...extra});
let e = key('ArrowUp', {altKey:true});
check('Alt+Up opens the session above', switchKeys(e) && e.prevented && __opened.pop() === 's-a');
e = key('ArrowUp', {altKey:true, target:{tagName:'TEXTAREA', closest:sel => sel.includes('monaco') ? {} : null}});
check('but not in the editor, where it moves a line', !switchKeys(e) && !e.prevented);
e = key('ArrowUp', {altKey:true, target:{tagName:'INPUT', id:'q', value:'typing', closest:() => null}});
check('nor in a box with text in it', !switchKeys(e));
e = key('k', {code:'KeyK', ctrlKey:true});
check('Ctrl+K opens the sessions alone', switchKeys(e) && e.prevented && __drops === 1);
e = key('p', {code:'KeyP', ctrlKey:true});
check('Ctrl+P is left to the palette', !switchKeys(e));
if(!ok) process.exit(1);
