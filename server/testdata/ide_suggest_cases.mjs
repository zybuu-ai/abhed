// Assertions on the workbench's next prompt, spliced in by suggest_page_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const ev = (seq, type, payload) => ({id: 'ev' + seq, seq, type, actor:'system', payload, created_at: new Date().toISOString()});
const q = $('q'); q.placeholder = 'Describe a change'; q.value = '';
current = 's1'; live = false; es = {};
let prevented = 0; const key = k => ({key: k, shiftKey: false, preventDefault(){ prevented++; }});

render(ev(1, 'suggestion.offered', {text: 'Run the tests\u202e now', turn: 1}));
check('the placeholder is the suggestion, escaped', q.placeholder === 'Run the tests⟨U+202E⟩ now');
check('the box itself stays empty', q.value === '');

await send();
check('Enter on the empty box sends nothing', __posted.length === 0);

check('a key other than Tab leaves it', takeNext(key('ArrowRight')) === false && q.value === '');
check('Tab takes it into the box', takeNext(key('Tab')) === true && q.value === 'Run the tests⟨U+202E⟩ now' && prevented === 1);
check('and sends nothing', __posted.length === 0);
check('the placeholder is the box\'s own again', q.placeholder === 'Describe a change');
check('a second Tab is the browser\'s', takeNext(key('Tab')) === false);

q.value = '';
render(ev(2, 'suggestion.offered', {text: 'Commit it'}));
q.value = 'my own words';
check('Tab with something typed is not taken', takeNext(key('Tab')) === false && q.value === 'my own words');
q.value = '';
render(ev(3, 'user.message', {text: 'next'}));
check('a new turn takes the suggestion away', q.placeholder === 'Describe a change' && q.dataset.next === '');
q.value = 'typing';
render(ev(4, 'suggestion.offered', {text: 'Late'}));
q.value = '';
check('one arriving over typing is not offered', q.placeholder === 'Describe a change');
live = true;
render(ev(5, 'suggestion.offered', {text: 'Stale'}));
check('one arriving during a run is not offered', q.placeholder === 'Describe a change');
live = false; q.value = '';
render(ev(6, 'suggestion.offered', {text: 'Then this'}));
render(ev(7, 'subagent.ask', {tool: 'bash'}));
check('an ask takes it away', q.placeholder === 'Describe a change');

if(!ok) process.exit(1);
