// Assertions driven against the console's real render() and approval card,
// spliced in above this file's contents by console_render_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const tick = () => new Promise(r => setTimeout(r, 5));
const cards = () => tx.querySelectorAll('.approve');

render({seq:1, type:'user.message', payload:{text:'delegate'}});
render({seq:2, type:'subagent.ask', payload:{session:'child', subagent:'clean up', request_id:'cev7', call_id:'c1',
  tool:'bash', args:{command:'touch made.txt'}, reason:'matches an ask rule', scope:'bash(touch *)'}});
check('a subagent\'s ask draws an approval card', cards().length === 1);
const card = cards()[0];
check('the card names the subagent and says Always allow is session-wide',
  card.textContent.includes('subagent clean up') && card.textContent.includes('whole session'));
check('the Always allow button says it is for this session',
  card.querySelectorAll('.no').some(b => b.textContent === 'Always allow in this session'));
card.querySelector('.yes').onclick(); await tick();
check('the answer names the subagent\'s request on the parent session',
  __posted.length === 1 && __posted[0].path === '/v1/sessions/s1/approve' && __posted[0].body.request_id === 'cev7' && __posted[0].body.approved === true);

render({seq:3, type:'subagent.ask', payload:{session:'child', subagent:'clean up', request_id:'cev9', call_id:'c1', tool:'bash', args:{command:'touch b'}}});
render({seq:4, type:'subagent.action', payload:{session:'child', call_id:'c1', tool:'bash', decision:'denied', by:'system', request_id:'cev9'}});
check('a subagent.action settles its card', !cards().some(c => c.isConnected && c.textContent.includes('touch b')));

if(!ok) process.exit(1);
