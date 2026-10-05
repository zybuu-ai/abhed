// Assertions driven against the workbench's real send path, live state and
// approval prompt, spliced in above this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const tick = (ms = 5) => new Promise(r => setTimeout(r, ms));
const ev = (seq, type, payload) => ({id: 'ev' + seq, seq, type, actor:'agent', payload, created_at: new Date().toISOString()});
const open = () => __root.querySelectorAll('.ask').filter(a => a.querySelector('.btns'));
const fresh = (id, on) => {
  __root.childNodes = []; sent.length = 0; queued.clear(); asks.clear(); calls.clear();
  current = id; live = on; lastSeq = endedSeq = 0; es = {}; __delays = [];
};
const write = seq => ev(seq, 'action.requested', {call_id:'w' + seq, tool:'write', args:{path:'test.md', content:'hi'}, requires_approval:true});

// The stream delivers the first message before the POST answers.
fresh('s1', false);
let post = __defer('POST /v1/sessions/s1/messages');
$('q').value = 'create test.md';
const sending = send(); await tick();
render(ev(1, 'user.message', {text:'create test.md', client_id: sent[0].cid}));
post.resolve({session_id:'s1'}); await sending;
render(write(2));
check('the first turn is live when its message is delivered first', live === true);
check('its approval is asked on the first turn', open().length === 1 && open()[0].dataset.call === 'w2');
render(ev(3, 'action.approved', {call_id:'w2', step:'reviewer', by:'reviewer'}));
check('the answered prompt is settled', open().length === 0 && asks.size === 0);

// The request can come before the POST answers, too.
fresh('s1', false);
post = __defer('POST /v1/sessions/s1/messages');
$('q').value = 'again';
const again = send(); await tick();
render(ev(1, 'user.message', {text:'again', client_id: sent[0].cid}));
render(write(2));
check('a request before the POST answers is asked', open().length === 1);
post.resolve({session_id:'s1'}); await again;
check('the late POST answer does not ask it twice', __root.querySelectorAll('.ask').length === 1);

// Send now: the POST answers, then the interrupted turn ends, then the new one starts.
fresh('s1', true);
const b = userBubble('instead', 'queued'); b.qid = 'q1'; queued.set('q1', b);
__defer('DELETE /v1/sessions/s1/queue/q1').resolve(null);
post = __defer('POST /v1/sessions/s1/messages');
const now = sendNow(b); await tick();
post.resolve({session_id:'s1'}); await now;
render(ev(1, 'session.ended', {reason:'user_interrupt', turns:1}));
render(ev(2, 'user.message', {text:'instead', client_id: b.cid}));
render(write(3));
check('Send now\'s turn is live after the old one ends', live === true);
check('Send now\'s approval is asked', open().length === 1);

// Opened mid-run from a list that still said idle: the replay ends one turn
// and reaches a request in the next, and the server says it waits for approval.
fresh('s2', false);
__sessions = [{id:'s2', state:'waiting_approval'}];
render(ev(1, 'user.message', {text:'first'}));
render(write(2));
render(ev(3, 'action.approved', {call_id:'w2', step:'reviewer'}));
render(ev(4, 'session.ended', {reason:'completed', turns:1}));
render(ev(5, 'user.message', {text:'second'}));
render(write(6)); lastSeq = 6;
check('history alone does not make the page live', live === false && open().length === 0);
await tick(700);
check('the server\'s waiting_approval makes it live', live === true);
check('only the pending request is asked', open().length === 1 && open()[0].dataset.call === 'w6');

// A request left in an ended run is never offered.
fresh('s3', false);
__sessions = [{id:'s3', state:'done'}];
render(ev(1, 'user.message', {text:'old'}));
render(write(2));
render(ev(3, 'session.ended', {reason:'error', turns:1})); lastSeq = 3;
await tick(700);
check('an ended run\'s request is not asked', live === false && open().length === 0 && asks.size === 0);

// An answer asked for before an end that has since been drawn is not
// trusted, even when it comes back after the answer that end asked for.
fresh('s4', false);
__sessions = [{id:'s4', state:'running'}]; __delays = [900];
render(ev(1, 'user.message', {text:'x'})); lastSeq = 1;
await tick(630);
render(ev(2, 'session.ended', {reason:'completed', turns:1})); lastSeq = 2;
__sessions = [{id:'s4', state:'done'}];
await tick(1100);
check('a stale running answer does not leave the page live', live === false);

// Interrupted with a prompt showing: the old prompt is settled, only the new
// request is open, and a click on the old one sends nothing.
fresh('s5', true);
render(write(1));
const oldYes = open()[0].querySelector('.btns').firstChild;
render(ev(2, 'session.ended', {reason:'user_interrupt', turns:1}));
const mine = userBubble('instead', 'pending'); sent.push(mine);
render(ev(3, 'user.message', {text:'instead', client_id: mine.cid}));
render(write(4));
check('after an interrupt only the new request is open', open().length === 1 && open()[0].dataset.call === 'w4');
check('the old prompt says it was not answered', __root.querySelectorAll('.ask')[0].textContent.includes('not answered · run ended'));
__posted.length = 0; oldYes.on.click(); await tick();
check('a click on the old prompt sends nothing', __posted.length === 0);
open()[0].querySelector('.btns').firstChild.on.click(); await tick();
check('the new prompt\'s answer names its request', __posted.length === 1 && __posted[0].body.request_id === 'ev4' && __posted[0].body.approved === true);

// Two tabs: another tab answers, and this one's prompt settles from the stream.
fresh('s6', true);
render(write(1));
const otherYes = open()[0].querySelector('.btns').firstChild;
render(ev(2, 'action.approved', {call_id:'w1', step:'reviewer', by:'reviewer'}));
check('a prompt answered in another tab settles', open().length === 0 && asks.size === 0);
__posted.length = 0; otherYes.on.click(); await tick();
check('and a late click on it sends nothing', __posted.length === 0);

// A "not running" answer asked for before this page's next turn began does
// not turn the page off; it asks again.
fresh('s7', false);
__sessions = [{id:'s7', state:'done'}]; __delays = [200];
render(ev(1, 'user.message', {text:'old'})); lastSeq = 1;
await tick(650);
__sessions = [{id:'s7', state:'running'}];
const next = userBubble('next', 'pending'); sent.push(next);
render(ev(2, 'user.message', {text:'next', client_id: next.cid})); lastSeq = 2;
await tick(250);
check('a stale "done" leaves the new turn live', live === true);
await tick(800);
check('and the page asks again', live === true);

// Replaying a history of answered prompts moves focus once, to the open one.
fresh('s8', true); __focused.length = 0;
render(write(1)); render(ev(2, 'action.approved', {call_id:'w1', step:'reviewer'}));
render(write(3)); render(ev(4, 'action.approved', {call_id:'w3', step:'reviewer'}));
render(write(5));
await tick(150);
check('focus moves once, to the open prompt', __focused.length === 1 && __focused[0].parentNode.parentNode.dataset.call === 'w5');

// The model reuses its call id in the next turn: the old prompt cannot answer
// the new request, and the new one names its own request.
fresh('s9', true);
const reused = (seq) => ev(seq, 'action.requested', {call_id:'call_0', tool:'write', args:{path:'x'}, requires_approval:true});
render(reused(1));
const staleYes = open()[0].querySelector('.btns').firstChild;
render(ev(2, 'session.ended', {reason:'user_interrupt', turns:1}));
const again2 = userBubble('again', 'pending'); sent.push(again2);
render(ev(3, 'user.message', {text:'again', client_id: again2.cid}));
render(reused(4));
__posted.length = 0; staleYes.on.click(); await tick();
check('a prompt with a reused call id sends nothing for the new request', __posted.length === 0);
open()[0].querySelector('.btns').firstChild.on.click(); await tick();
check('the new request is answered by its own id', __posted.length === 1 && __posted[0].body.request_id === 'ev4');

// While the run goes on, a 409 means it is not waiting on this request now:
// the prompt stays answerable, says so, asks the server again, and is settled
// by the record. Once the run has ended, a 409 settles it.
for(const msg of ['no approval is pending for this session', 'that approval is no longer pending']){
  fresh('s10', true);
  render(write(1));
  __defer('POST /v1/sessions/s10/approve').reject(Object.assign(new Error(msg), {status:409}));
  __routes.length = 0; open()[0].querySelector('.btns').firstChild.on.click(); await tick(700);
  check('"' + msg + '" during a run keeps the prompt answerable', open().length === 1 && asks.has('w1') &&
    !open()[0].querySelector('.btns').firstChild.disabled && open()[0].textContent.includes('Not taken'));
  check('and asks the server for the session\'s state', __routes.includes('GET /v1/sessions'));
  __posted.length = 0; open()[0].querySelector('.btns').firstChild.on.click(); await tick();
  check('a second answer is sent for the same request', __posted.length === 1 && __posted[0].body.request_id === 'ev1');
  render(ev(2, 'action.approved', {call_id:'w1', step:'default', by:'reviewer'}));
  check('and the record settles it', open().length === 0);
  fresh('s10', true);
  render(write(1)); live = false;
  __defer('POST /v1/sessions/s10/approve').reject(Object.assign(new Error(msg), {status:409}));
  open()[0].querySelector('.btns').firstChild.on.click(); await tick();
  check('"' + msg + '" after the run settles the prompt', open().length === 0);
}

// Another node runs the session: the prompt says where it can be answered.
fresh('s12', true);
render(write(1));
__defer('POST /v1/sessions/s12/approve').reject(Object.assign(new Error('this session is running on another node; route by session id'), {status:421, node:'node-b'}));
open()[0].querySelector('.btns').firstChild.on.click(); await tick();
check('a 421 settles the prompt and names the node', open().length === 0 && __root.querySelectorAll('.ask')[0].textContent.includes('node-b'));

// Too many answers waiting is busy, not refused: the prompt stays open.
fresh('s14', true);
render(write(1));
__defer('POST /v1/sessions/s14/approve').reject(Object.assign(new Error('too many answers are waiting for this session'), {status:429}));
open()[0].querySelector('.btns').firstChild.on.click(); await tick();
check('a 429 keeps the prompt and says to try again', open().length === 1 && open()[0].textContent.includes('Busy, try again'));

// Recorded after the run moved on: the prompt says the action did not run on it.
fresh('s13', true);
render(write(1));
__defer('POST /v1/sessions/s13/approve').resolve({recorded:true, applied:false});
open()[0].querySelector('.btns').firstChild.on.click(); await tick();
check('a recorded but unapplied answer settles the prompt', open().length === 0 && __root.querySelectorAll('.ask')[0].textContent.includes('moved on'));

// A request never shown is dropped when the next message arrives.
fresh('s11', false);
render(ev(1, 'user.message', {text:'old'}));
render(write(2));
render(ev(3, 'user.message', {text:'new'}));
check('an unshown request does not outlive its turn', asks.size === 0);
await tick(700);

// Send now while the server drains: the message is not withdrawn, since
// nothing it sends instead would be taken.
fresh('s15', true); __routes.length = 0;
const held = userBubble('while draining', 'queued'); held.qid = 'q9'; queued.set('q9', held);
__defer('GET /v1/health').resolve({status:'ok', draining:true});
await sendNow(held);
check('Send now during a drain leaves the message queued',
  !__routes.some(r => r.startsWith('DELETE')) && queued.get('q9') === held && held.classList.contains('queued'));
check('and says why', held.qnote.includes('shutting down') && held.qnote.includes('still queued'));

// The drain starts between the check and the send: the message is out of the
// queue, so the page says so and puts its text back in the box.
fresh('s16', true); $('q').value = '';
const late = userBubble('just too late', 'queued'); late.qid = 'q10'; queued.set('q10', late);
__defer('DELETE /v1/sessions/s16/queue/q10').resolve(null);
__defer('POST /v1/sessions/s16/messages').reject(Object.assign(new Error('server is shutting down; retry'), {status:503}));
await sendNow(late);
check('a 503 after the withdrawal says it is no longer queued',
  late.classList.contains('failed') && late.qnote.includes('no longer queued'));
check('and its text is back in the message box',
  $('q').value === 'just too late' && late.qnote.includes('back in the message box'));

// A drain that ends the run with a message still queued drops it; the bubble
// says it was not delivered, offers nothing, and its text is back in the box.
fresh('s17', true); $('q').value = '';
const dropped = userBubble('after the drain', 'queued'); dropped.qid = 'q11'; queued.set('q11', dropped); qbox.appendChild(dropped);
render(ev(8, 'message.dropped', {queue_id:'q11', client_id:dropped.cid, text:'after the drain', reason:'server shut down before it was delivered'}));
check('a dropped message is shown as not delivered',
  dropped.classList.contains('failed') && !dropped.classList.contains('queued') && dropped.qnote.startsWith('Not delivered — server shut down'));
// drawQueued offers Send now and Cancel only on a bubble still queued.
check('and no longer offers Send now or Cancel', !queued.has('q11') && !dropped.classList.contains('queued'));
check('and its text is back in the message box', $('q').value === 'after the drain' && dropped.qnote.includes('back in the message box'));

// A subagent's call waiting on the person is asked here, answered by its
// request id, and settled by the subagent.action that follows.
fresh('s18', true);
render(write(1)); render(ev(2, 'action.approved', {call_id:'w1', step:'reviewer'}));
render(ev(3, 'subagent.ask', {session:'child', subagent:'clean up', request_id:'cev7', call_id:'w1', tool:'bash', args:{command:'touch made.txt'}, reason:'ask rule'}));
check('a subagent\'s ask is put to the person', open().length === 1 && open()[0].dataset.call === 'subagent-cev7');
check('and says Always allow covers the whole session', open()[0].textContent.includes('whole session'));
__posted.length = 0; open()[0].querySelector('.btns').firstChild.on.click(); await tick();
check('its answer names the subagent\'s request', __posted.length === 1 && __posted[0].body.request_id === 'cev7' && __posted[0].body.approved === true);
render(ev(4, 'subagent.action', {session:'child', call_id:'w1', tool:'bash', decision:'allowed', by:'reviewer', request_id:'cev7'}));
check('and the subagent.action settles it', open().length === 0 && !asks.has('subagent-cev7'));

// A background subagent's ask outlives the run while background work is
// owed; the run's own ask ends with it. Its outcome or the closing end
// settles it, and an ask made after the run is offered too.
fresh('s19', true);
render(write(1));
render(ev(2, 'subagent.ask', {session:'child', subagent:'clean up', request_id:'cev21', call_id:'x1', tool:'bash', args:{command:'touch a'}, reason:'ask rule'}));
render(ev(3, 'session.ended', {reason:'max_turns', turns:3, background:1}));
check('after the run ends with background owed, the subagent\'s ask stays open',
  open().length === 1 && open()[0].dataset.call === 'subagent-cev21' && asks.has('subagent-cev21'));
check('and the run\'s own ask is settled', !asks.has('w1'));
render(ev(4, 'subagent.ask', {session:'child', subagent:'clean up', request_id:'cev22', call_id:'x2', tool:'bash', args:{command:'touch b'}, reason:'ask rule'}));
check('an ask made after the run is offered while background work is owed', open().some(a => a.dataset.call === 'subagent-cev22'));
__posted.length = 0; open().find(a => a.dataset.call === 'subagent-cev22').querySelector('.btns').firstChild.on.click(); await tick();
check('and answered by its request id', __posted.length === 1 && __posted[0].body.request_id === 'cev22');
render(ev(5, 'subagent.action', {session:'child', call_id:'x2', tool:'bash', decision:'allowed', by:'reviewer', request_id:'cev22'}));
check('its outcome settles it alone', !open().some(a => a.dataset.call === 'subagent-cev22') && open().some(a => a.dataset.call === 'subagent-cev21'));
render(ev(6, 'session.ended', {reason:'max_turns', turns:3, settled:true}));
check('the closing end settles what is left', open().length === 0 && asks.size === 0);
// A reopened session shows what was decided on a subagent's calls: a row for
// each, allowed or denied, with no card to answer.
fresh('s20', false);
render(ev(1, 'subagent.ask', {session:'child', subagent:'probe', request_id:'r1', call_id:'k1', tool:'bash', args:{command:'echo hi'}}));
render(ev(2, 'subagent.action', {session:'child', call_id:'k1', tool:'bash', decision:'allowed', by:'reviewer', request_id:'r1'}));
render(ev(3, 'subagent.ask', {session:'child', subagent:'probe', request_id:'r2', call_id:'k2', tool:'bash', args:{command:'date -u'}}));
render(ev(4, 'subagent.action', {session:'child', call_id:'k2', tool:'bash', decision:'denied', by:'reviewer', reason:'rejected', request_id:'r2'}));
render(ev(5, 'subagent.action', {session:'child', call_id:'k3', tool:'bash', subject:'reboot', decision:'denied', by:'policy', step:'deny', reason:'denied by rule', request_id:'r3'}));
{
  const rows = __root.childNodes.filter(n => n.className === 'call' || n.className === 'call denied'), t = __root.textContent;
  check('a replayed subagent call is drawn with its outcome', rows.length === 3 && t.includes('subagent probe') && t.includes('echo hi') && t.includes('allowed'));
  check('a denied one says so, with the reason', rows.filter(r => r.className.includes('denied')).length === 2 && t.includes('rejected') && t.includes('reboot'));
  check('and none is offered as an open card', open().length === 0 && asks.size === 0);
}

// A page whose run ended, a second tab say, follows the next turn another
// tab starts: it asks the server now and then, and opens the stream.
fresh('s22', false); es = null; __connected.length = 0;
__sessions = [{id:'s22', state:'idle'}];
watchIdle(); await tick();
check('an idle session is left idle', !live && __connected.length === 0);
__sessions = [{id:'s22', state:'running'}]; __routes.length = 0;
watchIdle(); await tick();
check('a turn started elsewhere is followed', live && __connected.length === 1 && __connected[0] === 's22');
check('by asking after that session alone, not the tenant\'s list',
  __routes.includes('GET /v1/sessions/s22/state') && !__routes.includes('GET /v1/sessions'));
es = {}; __routes.length = 0; live = false;
watchIdle(); await tick();
check('a page with a stream open does not ask', __routes.length === 0);

// A turn that started and ended between two asks is read back from the record
// when the server's turn count moves, rather than waiting for the next one.
fresh('s24', false); es = null; __connected.length = 0; idleTurns.clear();
__sessions = [{id:'s24', state:'idle', turns:2}];
watchIdle(); await tick(); __connected.length = 0; es = null;
watchIdle(); await tick();
check('an idle session whose count is unchanged is left alone', !live && __connected.length === 0);
__sessions = [{id:'s24', state:'idle', turns:3}];
watchIdle(); await tick();
check('a quick turn another tab ran is read back', !live && __connected.length === 1 && __connected[0] === 's24');

// An ask with no request id is not drawn as a card anyone could answer.
fresh('s23', true);
render(ev(1, 'subagent.ask', {session:'child', subagent:'x', call_id:'k9', tool:'bash', args:{command:'touch nid'}}));
askApproval({call_id:'w9', tool:'bash', args:{command:'ls'}}, undefined);
check('an ask with no request id draws no answerable card', open().length === 0 && __root.textContent.includes('no request id'));

// A pipeline step's ask names the pipeline asking.
fresh('s19', true);
render(ev(1, 'action.requested', {call_id:'step_1', tool:'bash', args:{command:'date -u > stamp.txt'}, requires_approval:true, via:'skill tide-audit pipeline'}));
check('a pipeline step\'s ask names the pipeline', open().length === 1 && open()[0].textContent.includes('Asked by skill tide-audit pipeline'));
render(ev(2, 'subagent.ask', {session:'child', subagent:'runner', request_id:'cev21', call_id:'step_2', tool:'bash', args:{command:'ls'}, via:'skill p pipeline'}));
check('and so does one a subagent\'s pipeline puts', open().some(a => a.textContent.includes('Asked by skill p pipeline') && a.textContent.includes('subagent runner')));

// A background start says it keeps running, and a call with no one-click scope says why.
fresh('s47', true);
render(ev(1, 'action.requested', {call_id:'bg1', tool:'bash', args:{command:'sleep 40; echo one', run_in_background:true}, requires_approval:true}));
render(ev(2, 'action.requested', {call_id:'fg1', tool:'bash', args:{command:'ls -la'}, requires_approval:true, scope:'bash(ls *)'}));
const bgCard = open().find(a => a.dataset.call === 'bg1'), fgCard = open().find(a => a.dataset.call === 'fg1');
check('a background start says it keeps running', !!bgCard && bgCard.textContent.includes('Starts in the background: it keeps running after this turn'));
check('and why it offers no Always allow', !!bgCard && bgCard.textContent.includes('No Always allow for this call'));
check('a foreground call with a scope says neither', !!fgCard && !fgCard.textContent.includes('in the background') && !fgCard.textContent.includes('No Always allow') && fgCard.textContent.includes('Always allow bash(ls *)'));

// A call's own text cannot hide what allowing runs: control and format
// characters are written out, and the prompt warns that they were there.
fresh('s30', true);
{
  const payload = 'touch pwned #\u200d\r\u001b[2K\u202e\u007f\u009b  $ ls -la';
  render(ev(1, 'action.requested', {call_id:'x1', tool:'bash', args:{command: payload}, requires_approval:true,
    reason:'why\r' + payload, scope:'bash(' + payload + ')', via:'via\u200b'}));
  const ask = open()[0];
  const shown = ask ? ask.textContent : '';
  const raw = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f\u200b-\u200f\u202a-\u202e\u2066-\u2069\ufeff]/;
  check('the prompt shows hidden characters instead of obeying them',
    !!ask && !raw.test(shown) && shown.includes('touch pwned') && shown.includes('⟨U+200D⟩') && shown.includes('⟨U+000D⟩'));
  check('the prompt warns that the call carries hidden characters', shown.includes('hidden or control characters'));
  const row = calls.get('x1').node.querySelector('.subj').textContent;
  check('the call row shows them too', row.includes('⟨U+202E⟩') && !raw.test(row));
  render(ev(2, 'action.requested', {call_id:'x2', tool:'bash', args:{command:'printf "a\tb"'}, requires_approval:true}));
  check('a plain call draws no warning', open().length === 2 && !open()[1].textContent.includes('hidden or control characters'));
}

// The warning reads every value in the call, not only the subject it draws.
fresh('s31', true);
{
  const calls = [
    ['write', {path:'a.txt', content: Array.from({length:25}, (_, i) => i === 19 ? 'x\u202ey' : 'line').join('\n')}],
    ['bash', {command:'ls\rrm -rf x', description:'list'}],
    ['k8s_apply', {action:'apply', manifest:'{"kind":"ConfigMap","data":{"k":"\\u001b[2J"}}'}],
    ['task', {description:'look', prompt:'a\u200db'}],
  ];
  calls.forEach(([tool, args], i) => render(ev(1 + i, 'action.requested', {call_id:'h' + i, tool, args, requires_approval:true})));
  const warned = open().map(a => a.textContent.includes('hidden or control characters'));
  check('a hidden character anywhere in the args raises the warning: ' + warned, warned.length === 4 && warned.every(Boolean));
}

// A run of spaces is counted on the prompt, and a field the prompt does not
// draw is shown when it is what carries the hidden characters.
fresh('s32', true);
{
  render(ev(1, 'action.requested', {call_id:'w1', tool:'bash', args:{command:'git status --short' + ' '.repeat(260) + '&& tar czf p.tgz internal'}, requires_approval:true}));
  const t = open()[0] ? open()[0].textContent : '';
  check('a long run of spaces is counted on the prompt and warned',
    t.includes('git status --short⟨260 spaces⟩&& tar czf p.tgz internal') && t.includes('hidden or control characters'));
  render(ev(2, 'action.requested', {call_id:'w2', tool:'bash', args:{command:'ls', description:'list\u202e files'}, requires_approval:true}));
  const d = open()[1] ? open()[1].textContent : '';
  check('a hidden character only in the description is shown with its field', d.includes('description: list⟨U+202E⟩ files'));
}

// A non-ASCII space reads as a space: it is written out and warned, and
// ordinary spaces are left as they are.
fresh('s33', true);
{
  render(ev(1, 'action.requested', {call_id:'n1', tool:'bash', args:{command:'rm\u00a0-rf /tmp/x'}, requires_approval:true}));
  const t = open()[0] ? open()[0].textContent : '';
  check('a no-break space is shown and warned: ' + t, t.includes('rm\u27e8U+00A0\u27e9-rf /tmp/x') && !t.includes('\u00a0') && t.includes('hidden or control characters'));
  render(ev(2, 'action.requested', {call_id:'n2', tool:'bash', args:{command:'echo a b c'}, requires_approval:true}));
  const p = open()[1] ? open()[1].textContent : '';
  check('ordinary spaces are untouched and not warned', p.includes('echo a b c') && !p.includes('U+0020') && !p.includes('hidden or control characters'));
}

// A background result wakes the session while the page sits on the open
// stream: the woken turn is drawn as it streams, marked with the task it
// continues from, and its ask is offered, with no reload.
fresh('s40', false);
{
  render(ev(1, 'user.message', {text:'start a scan'}));
  render(ev(2, 'subagent.spawned', {task_id:'t1', description:'scan logs', background:true}));
  render(ev(3, 'session.ended', {reason:'completed', turns:1, background:1}));
  check('the task shows as running', $('s-bg').hidden === false && $('s-bg').textContent.includes('scan logs'));
  check('the page is not live between turns', live === false && bgLive === true);
  render(ev(4, 'session.woken', {by:'policy', wake_mode:'auto', task_ids:['t1']}));
  render(ev(5, 'subagent.notice', {task_id:'t1', description:'scan logs', status:'completed', content:'three errors'}));
  check('the woken turn is marked with its task', __root.textContent.includes('continuing with results from scan logs'));
  check('the woken turn is live without a reload', live === true);
  check('the finished task leaves the running list', $('s-bg').hidden === true);
  render(write(6));
  check('the woken turn\'s ask is offered', open().length === 1 && open()[0].dataset.call === 'w6');
  render(ev(7, 'action.denied', {call_id:'w6', step:'reviewer', reason:'no'}));
  render(ev(9, 'session.ended', {reason:'completed', turns:2, background:0}));
  check('the woken turn ends like any other', live === false);
}

// The sender's own bubble draws what was typed through the helper, and keeps
// the raw text to put back in the box if the send fails.
fresh('s41', false);
{
  const typed = 'fix\u202etxt.exe\u001b[2J\n    indented\tline';
  const mine = userBubble(typed, 'pending'); sent.push(mine);
  render(ev(1, 'user.message', {text: typed, client_id: mine.cid}));
  const shown = __root.textContent;
  check('the sender\'s bubble reveals hidden characters', shown.includes('fix⟨U+202E⟩txt.exe⟨U+001B⟩[2J\n    indented\tline') && !shown.includes('\u202e') && !shown.includes('\u001b'));
  check('the sender\'s bubble is the one claimed', __root.childNodes.length === 1 && __root.childNodes[0] === mine);
  check('the bubble keeps the raw text for a retry', mine.text === typed);
}

// Stop stays while background work runs with no turn going, since it stops that work too.
fresh('s42', true); bgLive = false; bgTasks.clear();
{
  setLive(true);
  check('Stop shows while a run is live', $('stop').hidden === false);
  render(ev(1, 'shell.started', {shell_id:'sh1', description:'watch the build'}));
  render(ev(2, 'session.ended', {reason:'completed', turns:1, background:1}));
  check('Stop stays while background work runs with no turn', live === false && $('stop').hidden === false && /background/.test($('stop').title));
  render(ev(3, 'shell.ended', {shell_id:'sh1', state:'exited'}));
  render(ev(4, 'session.ended', {reason:'completed', turns:1, settled:true}));
  check('and goes once the background work has settled', $('stop').hidden === true);
}

// A task whose notice is not yet recorded, as after a restart, is not shown running
// once the record says it returned or the session settled.
fresh('s43', false); bgLive = false; bgTasks.clear();
{
  render(ev(1, 'subagent.spawned', {task_id:'t1', description:'scan logs', background:true}));
  render(ev(2, 'subagent.spawned', {task_id:'t2', description:'index docs', background:true}));
  render(ev(3, 'session.ended', {reason:'completed', turns:1, background:2}));
  check('both tasks show running', $('s-bg').textContent.includes('2 background'));
  render(ev(4, 'subagent.returned', {task_id:'t1', reason:'shutdown'}));
  check('a returned task leaves the running list', $('s-bg').textContent.includes('1 background') && !$('s-bg').textContent.includes('scan logs'));
  render(ev(5, 'session.ended', {reason:'shutdown', turns:1, settled:true}));
  check('a settled end leaves nothing shown running', $('s-bg').hidden === true && !bgLive);
}
fresh('s44', false); bgLive = false; bgTasks.clear();
{
  render(ev(1, 'subagent.spawned', {task_id:'t1', description:'scan logs', background:true}));
  render(ev(2, 'session.ended', {reason:'shutdown', turns:1, background:0}));
  check('an end that owes no background work shows none running', $('s-bg').hidden === true);
}

// Comments written on lines of the changes go with the next message, after what was typed,
// each with its file, line and that line's text; one that fails to send comes back.
fresh('s45', false);
{
  check('an empty comment is not kept', addNote('src/a.go', 3, 'x := 1', '   ') === false && reviewNotes.length === 0);
  addNote('src/a.go', 12, '\treturn `x`', 'why not\nreturn early?');
  addNote('lib\u202e/b.go', 4, '', 'rename this');
  const chips = $('rnotes').childNodes;
  check('each comment is shown above the box', chips.length === 2 && chips[0].textContent.startsWith('src/a.go:12'));
  check('a file name is drawn with its hidden characters revealed', chips[1].textContent.includes('lib⟨U+202E⟩/b.go:4') && !chips[1].textContent.includes('\u202e'));
  let post = __defer('POST /v1/sessions/s45/messages');
  $('q').value = 'please review';
  const going = send(); await tick();
  check('sending takes the comments', reviewNotes.length === 0 && $('rnotes').childNodes.length === 0);
  post.resolve({session_id:'s45'}); await going;
  const body = __posted.find(x => x.route === 'POST /v1/sessions/s45/messages').body.prompt;
  check('the message carries them after what was typed', body === "please review\n\nReview comments on the changes:\n- `src/a.go` line 12 (`return 'x'`): why not\n  return early?\n- `lib\u202e/b.go` line 4: rename this");
  check('the bubble names them', sent[0].textContent.includes('comment on src/a.go:12: why not'));

  // Comments alone can be sent; a failed send puts them back.
  fresh('s46', false); addNote('a.txt', 1, 'hi', 'typo');
  post = __defer('POST /v1/sessions/s46/messages');
  const lone = send(); await tick();
  post.reject(Object.assign(new Error('the server is busy'), {status:503})); await lone;
  check('comments with nothing typed are sent', __routes.includes('POST /v1/sessions/s46/messages'));
  check('a send that failed puts the comments back', reviewNotes.length === 1 && reviewNotes[0].text === 'typo');
  reviewNotes = []; drawNotes();
}

if(!ok) process.exit(1);
