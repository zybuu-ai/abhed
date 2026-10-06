// The console's chat list, the address and the switcher, driven against the
// real functions spliced in above by console_render_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const tick = (ms = 5) => new Promise(r => setTimeout(r, ms));
const at = q => { location.search = q; location.href = 'https://h/console' + q; };
const NBSP = String.fromCharCode(0xa0);
const list = [
  {id:'s-a', prompt:'fix the retry loop', created:'2026-10-06T08:00:00Z', updated:'2026-10-06T09:30:00Z', state:'done'},
  {id:'s-b', prompt:'terraform cleanup', title:'Zebra' + NBSP + 'renamed', created:'2026-10-06T08:10:00Z', updated:'2026-10-06T08:15:00Z', state:'done'},
  {id:'s-c', prompt:'release notes draft', created:'2026-10-06T08:20:00Z', state:'done'},
];

// Order and search.
const sorted = sortSessions(list);
check('the list is in order of last activity, the start standing in for none', sorted.map(s => s.id).join() === 's-a,s-c,s-b');
check('a title is searched', matchSession(list[1], 'zebra') && matchSession(list[1], 'ZEBRA'));
check('every word must be found, in any order', matchSession(list[1], 'renamed zebra') && !matchSession(list[1], 'zebra retry'));
check('the first message is searched too', matchSession(list[0], 'retry') && matchSession(list[1], 'terraform'));
check('a title is shown with its hidden characters written out', sessionName(list[1]) === 'Zebra⟨U+00A0⟩renamed');
check('and is found as shown', matchSession(list[1], 'u+00a0'));

// The address.
at('?s=s-b');
check('the address names the chat to open', sessionFromURL() === 's-b');
at('?s=../../x');
check('an address that is not an id is ignored', sessionFromURL() === null);
at('');
setURL('s-c');
check('opening a chat puts it in the address', location.search === '?s=s-c' && __urls.length === 1);
check('and the workbench link opens the same one', $('wblink').href === '/ide?s=s-c');
setURL('s-c');
check('the same chat again writes no history', __urls.length === 1);
setURL(null);
check('a new chat leaves the address', location.search === '' && $('wblink').href === '/ide');

// Previous and next.
sessionsSeen = sorted; $('search').value = '';
current = 's-c';
check('Alt+Up goes to the chat above', neighbour(-1).id === 's-a');
check('Alt+Down to the one below', neighbour(1).id === 's-b');
current = 's-b';
check('past the end there is none', neighbour(1) === null);
current = null;
check('with no chat open, down takes the first', neighbour(1).id === 's-a');

// The keys.
const key = (k, extra = {}) => ({key:k, metaKey:false, ctrlKey:false, altKey:false, shiftKey:false, target:{tagName:'BODY'}, preventDefault(){ this.prevented = true; }, ...extra});
current = 's-a'; __opened.length = 0;
let e = key('ArrowDown', {altKey:true});
check('Alt+Down switches', switchKeys(e) && e.prevented && __opened[0] === 's-c');
e = key('ArrowDown', {altKey:true, target:{tagName:'TEXTAREA', id:'q', value:'half a message'}});
check('but not in a box with text in it', !switchKeys(e) && !e.prevented);
e = key('k', {ctrlKey:true});
check('Ctrl+K opens the switcher', switchKeys(e) && e.prevented && !$('qs').hidden);

// The switcher.
$('qs').hidden = false; $('qsq').value = ''; current = 's-a'; qsSel = 0;
drawSwitcher();
let rows = $('qsl').childNodes.filter(n => n.tag === 'button');
check('the switcher lists every chat', rows.length === 3);
check('the open chat is marked', rows[0].textContent.includes('● open') && !rows[1].textContent.includes('open'));
check('and the selection starts on the next one', rows[1].attrs['aria-selected'] === 'true');
$('qsq').value = 'zebra'; qsSel = 0; drawSwitcher();
check('it filters by title', $('qsl').childNodes.filter(n => n.tag === 'button').length === 1 && $('qsl').textContent.includes('Zebra⟨U+00A0⟩renamed'));
$('qsq').value = 'nothing like it'; drawSwitcher();
check('and says when nothing matches', $('qsl').textContent === 'No chats match.');

// The header.
headTitle(list[1]);
check('the header shows the title, not the id', $('sid').textContent === 'Zebra⟨U+00A0⟩renamed' && $('sid').title.includes('s-b') && !$('copyid').hidden);

// The rail.
__list = list;
$('search').value = 'nothing like it'; $('list').dataset.sig = '';
await refresh();
check('a search with no match says so', $('list').textContent.includes('No chats match') && $('count').textContent === '0 of 3');
$('search').value = 'zebra'; $('list').dataset.sig = '';
await refresh();
check('a search finds a renamed chat by its title', $('list').querySelectorAll('item').length === 1 && $('count').textContent === '1 of 3');
$('search').value = ''; $('list').dataset.sig = '';
await refresh();
check('the rail is in order of last activity', $('list').querySelectorAll('item').map(r => r.dataset.id).join() === 's-a,s-c,s-b');

// Rename in place.
current = 's-c';
renameInPlace($('sid'), list[2]);
let box = $('sid').childNodes.find(n => n.tag === 'input');
check('rename offers a box', box && box.tag === 'input' && box.value === '');
box.value = '  Release notes  ';
box.on.keydown({key:'Enter', preventDefault(){}, stopPropagation(){}});
await tick(20);
check('Enter saves the title', __posts.length === 1 && __posts[0][0] === '/v1/sessions/s-c/title' && __posts[0][1].title === 'Release notes');
check('and the header shows it', $('sid').textContent === 'Release notes');
renameInPlace($('sid'), list[1]);
box = $('sid').childNodes.find(n => n.tag === 'input');
check('a title with hidden characters is not seeded raw', box.value === '' && box.placeholder === 'Zebra⟨U+00A0⟩renamed');
box.on.keydown({key:'Enter', preventDefault(){}, stopPropagation(){}});
await tick(20);
check('and leaving the empty box keeps it', __posts.length === 1);
renameInPlace($('sid'), list[0]);
box = $('sid').childNodes.find(n => n.tag === 'input'); box.value = 'never';
box.on.keydown({key:'Escape', preventDefault(){}, stopPropagation(){}});
await tick(20);
check('Esc keeps the old title', __posts.length === 1);

// Restoring from the address.
current = null; __opened.length = 0; at('?s=s-b');
await restoreFromURL();
check('a reload opens the chat in the address', __opened[0] === 's-b');
current = null; __opened.length = 0; at('?s=s-gone'); __states['s-gone'] = null;
await restoreFromURL();
check('a chat that is gone is said so, and dropped from the address', !__opened.length && __notes.some(n => n.includes('not one you can open')) && location.search === '');
current = null; __opened.length = 0; at('?s=s-older'); __states['s-older'] = {state:'done'};
await restoreFromURL();
check('a chat beyond the list is asked for by id', __opened[0] === 's-older');
if(!ok) process.exit(1);
