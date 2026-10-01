// Assertions driven against the console's real render() and approval card,
// spliced in above this file's contents by console_render_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const tick = (ms = 5) => new Promise(r => setTimeout(r, ms));
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
await tick(900);
check('a 409 with no stream open reopens it, so the record can settle the card', __connected.length >= 1 && __connected[0] === 's1');
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

// The warning reads every value in the call, not the JSON text drawn from it.
{
  const calls = [
    ['write', {path:'a.txt', content: Array.from({length:25}, (_, i) => i === 19 ? 'x\u202ey' : 'line').join('\n')}],
    ['bash', {command:'ls\rrm -rf x', description:'list'}],
    ['k8s_apply', {action:'apply', manifest:'{"kind":"ConfigMap","data":{"k":"\\u001b[2J"}}'}],
    ['task', {description:'look', prompt:'a\u200db'}],
  ];
  calls.forEach(([tool, args], i) => render({seq:60 + i, id:'ev6' + i, type:'action.requested', payload:{call_id:'h' + i, tool, requires_approval:true, args}}));
  const warned = calls.map((_, i) => approvals.has('h' + i) && approvals.get('h' + i).textContent.includes('hidden or control characters'));
  check('a hidden character anywhere in the args raises the warning: ' + warned, warned.every(Boolean));
}

// A run of spaces cannot push the tail of a command out of the card: it is
// counted, the card warns, and indentation after a newline is left alone.
{
  render({seq:70, id:'ev70', type:'action.requested', payload:{call_id:'w70', tool:'bash', requires_approval:true,
    args:{command:'git status --short' + ' '.repeat(260) + '&& tar czf pwned.tgz internal', description:'status'}}});
  const card = approvals.get('w70'), pre = card ? card.textContent : '';
  check('a long run of spaces is counted in the card', pre.includes('git status --short\u27e8260 spaces\u27e9&& tar czf pwned.tgz internal') && !pre.includes('     '));
  check('a long run of spaces raises the warning', !!card && card.textContent.includes('hidden or control characters'));
  render({seq:71, id:'ev71', type:'action.requested', payload:{call_id:'w71', tool:'bash', requires_approval:true, args:{command:'a\t\tb'}}});
  check('a run of tabs is counted and warned', approvals.get('w71').textContent.includes('a\u27e82 tabs\u27e9b') && approvals.get('w71').textContent.includes('hidden or control'));
  render({seq:72, id:'ev72', type:'action.requested', payload:{call_id:'w72', tool:'bash', requires_approval:true,
    args:{command:"python3 - <<'EOF'\nfor x in y:\n        print(x)\nEOF"}}});
  const py = approvals.get('w72');
  check('indentation inside a heredoc is drawn as it is, with no warning',
    py.textContent.includes('\\n        print(x)') && !py.textContent.includes('hidden or control'));
  render({seq:73, id:'ev73', type:'action.requested', payload:{call_id:'w73', tool:'bash', requires_approval:true, args:{command:'echo \u3164\u2800 a\ufe0f \u2764\ufe0f'}}});
  const fill = approvals.get('w73').textContent;
  check('characters that draw nothing are shown: ' + fill, fill.includes('\u27e8U+3164\u27e9\u27e8U+2800\u27e9') && fill.includes('a\u27e8U+FE0F\u27e9') && fill.includes('\u2764\ufe0f'));
}

if(!ok) process.exit(1);
