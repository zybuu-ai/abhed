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

// A subagent's start and finish read in the turn, before the answer after them.
render({seq:30, type:'user.message', payload:{text:'fan out'}});
render({seq:31, type:'action.requested', payload:{call_id:'t1', tool:'task', args:{prompt:'x', description:'probe'}}});
render({seq:32, type:'subagent.spawned', payload:{session:'k1', description:'probe'}});
render({seq:33, type:'subagent.returned', payload:{session:'k1', reason:'completed'}});
render({seq:34, type:'observation', payload:{call_id:'t1', tool:'task', content:'found it'}});
render({seq:35, type:'agent.message', payload:{text:'THE-ANSWER'}});
{
  const all = tx.textContent, answer = all.lastIndexOf('THE-ANSWER');
  check('subagent notes are drawn before the answer that follows them',
    answer > 0 && all.lastIndexOf('subagent started: probe') < answer && all.lastIndexOf('subagent finished: completed') < answer);
}

// An ask with no request id is not drawn as a card anyone could answer.
{
  const before = cards().length;
  render({seq:40, type:'subagent.ask', payload:{session:'child', subagent:'x', call_id:'c40', tool:'bash', args:{command:'touch nid'}}});
  check('an ask with no request id draws no answerable card', cards().length === before && tx.textContent.includes('no request id'));
  // The card itself refuses to be drawn without the id its answer must name.
  approval({call_id:'c41', tool:'bash', args:{command:'touch direct'}}, undefined);
  approval({call_id:'c42', tool:'bash', args:{command:'touch empty'}}, '');
  check('the approval card is never drawn without a request id', cards().length === before && !approvals.has('c41') && !approvals.has('c42'));
}

// A pipeline step's ask names the pipeline asking.
render({seq:4, type:'subagent.ask', payload:{session:'child', subagent:'runner', request_id:'cev10', call_id:'c4',
  tool:'bash', args:{command:'date -u > stamp.txt'}, via:'skill tide-audit pipeline'}});
check('a pipeline step\'s ask names the pipeline',
  cards().some(c => c.isConnected && c.textContent.includes('Asked by skill tide-audit pipeline')));

// While the run goes on, a 409 keeps the card answerable for the record to
// settle; after the run it retires the card.
globalThis.__reject = null;
const rejecting = async (path, opts) => { if(__reject){ const e = new Error(__reject); __reject = null; throw e; } __posted.push({path, body: JSON.parse(opts.body)}); return null; };
render({seq:5, type:'subagent.ask', payload:{session:'child', subagent:'clean up', request_id:'cev11', call_id:'c5', tool:'bash', args:{command:'touch q'}}});
const queuedCard = cards().find(c => c.textContent.includes('touch q'));
__api = rejecting; __reject = 'that approval is no longer pending';
queuedCard.querySelector('.yes').onclick(); await tick();
check('a 409 during a run keeps the card, answerable, and says so',
  queuedCard.isConnected && !queuedCard.querySelector('.yes').disabled && queuedCard.textContent.includes('Not taken'));
__posted.length = 0; queuedCard.querySelector('.yes').onclick(); await tick();
check('a second answer names the same request', __posted.length === 1 && __posted[0].body.request_id === 'cev11');
render({seq:6, type:'subagent.action', payload:{session:'child', call_id:'c5', tool:'bash', decision:'allowed', by:'reviewer', request_id:'cev11'}});
check('and the record settles it', !queuedCard.isConnected);
render({seq:7, type:'subagent.ask', payload:{session:'child', subagent:'clean up', request_id:'cev12', call_id:'c6', tool:'bash', args:{command:'touch r'}}});
const lateCard = cards().find(c => c.textContent.includes('touch r'));
live = false; __reject = 'that approval is no longer pending';
lateCard.querySelector('.yes').onclick(); await tick();
check('a 409 after the run retires the card', !lateCard.isConnected);

// A call's own text cannot hide what approving runs: control and format
// characters are written out, and the card warns that they were there.
{
  const payload = 'touch pwned #\u200d\r\u001b[2K\u202e\u007f\u009b  $ ls -la';
  render({seq:50, id:'ev50', type:'action.requested', payload:{call_id:'c50', tool:'bash', requires_approval:true,
    args:{command: payload, description:'x\r\u001b[2Kls'}, reason:'why\r' + payload, scope:'bash(' + payload + ')', via:'via\u200b'}});
  const card = cards().find(c => c.isConnected && c.textContent.includes('touch pwned'));
  const shown = card ? card.textContent + card.querySelectorAll('.no').map(b => b.title || '').join('') : '';
  const row = card ? card.parentNode.querySelector('.arg').textContent : '';
  const raw = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f\u200b-\u200f\u202a-\u202e\u2066-\u2069\ufeff]/;
  check('the approval card shows hidden characters instead of obeying them',
    !!card && !raw.test(shown) && shown.includes('⟨U+200D⟩') && shown.includes('⟨U+202E⟩') && shown.includes('⟨U+000D⟩'));
  check('the card warns that the call carries hidden characters', shown.includes('hidden or control characters'));
  check('the tool row shows them too', row.includes('⟨U+000D⟩') && !raw.test(row));
  render({seq:51, id:'ev51', type:'action.requested', payload:{call_id:'c51', tool:'bash', requires_approval:true, args:{command:'printf "a\tb\n"'}}});
  const plain = cards().find(c => c.isConnected && c.textContent.includes('printf'));
  check('a plain call draws no warning', !!plain && !plain.textContent.includes('hidden or control characters'));
}

if(!ok) process.exit(1);
