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

if(!ok) process.exit(1);
