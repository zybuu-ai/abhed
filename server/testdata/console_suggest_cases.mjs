// Assertions on the console's next prompt, spliced in by suggest_page_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const box = $('q'); box.value = '';
live = false;  // the suggestion follows the run's end
let prevented = 0; const key = k => ({key: k, shiftKey: false, preventDefault(){ prevented++; }});

render({seq: 1, type: 'suggestion.offered', payload: {text: 'Add a test\u0007 for it'}});
check('the placeholder is the suggestion, escaped', box.placeholder === 'Add a test⟨U+0007⟩ for it');
check('Tab takes it into the box', takeNext(key('Tab')) === true && box.value === 'Add a test⟨U+0007⟩ for it' && __grown === 1);
check('the placeholder is the box\'s own again', box.placeholder === 'Ask anything');
box.value = '';
render({seq: 2, type: 'suggestion.offered', payload: {text: 'Ship it'}});
render({seq: 3, type: 'user.message', payload: {text: 'go'}});
check('a new turn takes it away', box.placeholder === 'Ask anything');
live = true;
render({seq: 4, type: 'suggestion.offered', payload: {text: 'Stale'}});
check('one arriving during a run is not offered', box.placeholder === 'Ask anything');
live = false; box.value = 'typing';
render({seq: 5, type: 'suggestion.offered', payload: {text: 'Late'}});
box.value = '';
check('one arriving over typing is not offered', box.placeholder === 'Ask anything');
box.value = '';
render({seq: 6, type: 'suggestion.offered', payload: {text: 'Then this'}});
render({seq: 7, type: 'subagent.ask', payload: {tool: 'bash'}});
check('an ask takes it away', box.placeholder === 'Ask anything');

if(!ok) process.exit(1);
