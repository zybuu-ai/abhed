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

// Either 409 settles the prompt: the server will not take an answer for it.
for(const msg of ['no approval is pending for this session', 'that approval is no longer pending']){
  fresh('s10', true);
  render(write(1));
  __defer('POST /v1/sessions/s10/approve').reject(Object.assign(new Error(msg), {status:409}));
  open()[0].querySelector('.btns').firstChild.on.click(); await tick();
  check('"' + msg + '" settles the prompt', open().length === 0);
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

if(!ok) process.exit(1);
