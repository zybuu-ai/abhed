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

// A background subagent's ask outlives the run while background work is
// owed: the run's end leaves it answerable, its own outcome or the closing
// end settles it. The run's own ask ends with the run.
const answerable = text => cards().some(c => c.isConnected && c.textContent.includes(text) && c.querySelector('.yes'));
render({seq:5, type:'subagent.ask', payload:{session:'child', subagent:'clean up', request_id:'cev11', call_id:'c1', tool:'bash', args:{command:'touch later'}}});
render({seq:6, type:'action.requested', payload:{call_id:'own1', tool:'write', args:{path:'x.md'}, requires_approval:true}});
check('the run\'s own ask is drawn', answerable('x.md'));
render({seq:7, type:'session.ended', payload:{reason:'max_turns', turns:3, background:1}});
check('after a run ends with background owed, the subagent\'s ask stays answerable', answerable('touch later'));
check('while the run\'s own ask is settled', !answerable('x.md'));
cards().find(c => c.isConnected && c.textContent.includes('touch later')).querySelector('.yes').onclick(); await tick();
check('and it is answered by its request id', __posted.at(-1).body.request_id === 'cev11');
render({seq:8, type:'subagent.action', payload:{session:'child', call_id:'c1', tool:'bash', decision:'allowed', by:'reviewer', request_id:'cev11'}});
check('its own outcome settles it', !answerable('touch later'));
render({seq:9, type:'subagent.ask', payload:{session:'child', subagent:'clean up', request_id:'cev12', call_id:'c2', tool:'bash', args:{command:'touch never'}}});
check('a later ask while background work is owed is answerable', answerable('touch never'));
render({seq:10, type:'session.ended', payload:{reason:'max_turns', turns:3, settled:true}});
check('the closing end settles what is left', !answerable('touch never'));
render({seq:11, type:'subagent.ask', payload:{session:'child', subagent:'s', request_id:'cev13', call_id:'c3', tool:'bash', args:{command:'touch gone'}}});
render({seq:12, type:'session.ended', payload:{reason:'completed', turns:4, background:0}});
check('an end that owes nothing settles a subagent\'s ask too', !answerable('touch gone'));

if(!ok) process.exit(1);
