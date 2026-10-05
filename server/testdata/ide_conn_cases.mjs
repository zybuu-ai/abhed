// Assertions driven against the workbench's connection indicator, spliced in
// above this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const tick = (ms = 60) => new Promise(r => globalThis.setTimeout(r, ms));
const shown = () => $('s-conn').textContent;
const last = () => __streams[__streams.length - 1];

// A live session's stream drops and the server has gone.
connect('s1'); last().onopen();
check('an open stream says connected', shown() === '● connected');
__up = false; last().onerror();
check('a dropped live stream says reconnecting at once', shown() === '● reconnecting…');
await tick();
check('and offline once the server does not answer', shown() === '● offline');
await tick();
check('a retry that fails keeps it offline', shown() === '● offline');
__up = true; await tick(); last().onopen && last().onopen();
check('the reopened stream says connected again', shown() === '● connected');

// A finished session's replay closes while the server is still there.
live = false; connect('s1'); last().onopen(); last().onerror(); await tick();
check('a replay that ends does not claim the server has gone', shown() === '● connected');
connect('s1'); last().onopen(); __up = false; last().onerror(); await tick();
check('a replay that drops with the server gone says offline', shown() === '● offline');
__up = true; await tick();
check('and connected once the server answers', shown() === '● connected');

// A shell's stream closes with the server gone.
const t = {term:{write(){}, reset(){}}, mode:'shell'};
attach(t, 'p1'); __up = false; last().readyState = EventSource.CLOSED; last().onerror(); await tick();
check('a shell stream that drops with the server gone says offline', shown() === '● offline');
__up = true; await tick();

// Any request that cannot reach the server says so, too.
__up = false; try{ await api('/v1/sessions'); }catch{} await tick();
check('a request that cannot reach the server says offline', shown() === '● offline');
__up = true; await tick();
check('and it recovers on its own', shown() === '● connected');

// The longer the server is away, the less often it is asked, up to 30 s.
__up = false; __waits.length = 0; try{ await api('/v1/sessions'); }catch{} await tick(400);
const backoff = __waits.filter(ms => ms >= 3000);
check('the probe backs off from 3 s to 30 s', backoff.slice(0, 6).join(',') === '3000,6000,12000,24000,30000,30000');
__up = true; await tick(200);
check('and still recovers', shown() === '● connected');

// A hidden tab does not ask at all, and asks again once it is shown.
document.hidden = true; __up = false; __fetches = 0;
try{ await api('/v1/sessions'); }catch{} await tick();
check('a hidden tab does not probe', __fetches === 1 && typeof __docOn.visibilitychange === 'function');
document.hidden = false; __docOn.visibilitychange(); await tick(10);
check('and probes when it is shown', __fetches === 2 && shown() === '● offline');
__up = true; await tick(200);

// An idle page, with no stream open, notices the server leave on its own.
es = null; setConn(true); __up = false; connIdle(); await tick();
check('an idle page says offline once the server has gone', shown() === '● offline');
__up = true; await tick(200);
check('and connected once it is back', shown() === '● connected');
// A page with a stream open leaves it to the stream.
es = {readyState:1}; __fetches = 0; __up = false; connIdle(); await tick();
check('an open stream is not probed on the timer', __fetches === 0 && shown() === '● connected');
es = null; __up = true;

// A 401 means the sign-in ended: say so, with the way back, and stop the run's indicator.
live = true; __status = 401; __liveOff = 0; __added.length = 0;
try{ await api('/v1/sessions'); }catch{}
const endedNote = __added.find(n => n.textContent.startsWith('Your sign-in ended.'));
check('a 401 says the sign-in ended', !!endedNote && shown() === '● signed out');
check('and links to sign in again, back to the workbench', !!endedNote && endedNote.childNodes.some(c => c.href === '/?return=/ide'));
check('and stops the run', __liveOff === 1 && !live);
try{ await api('/v1/sessions'); }catch{}
check('and says it once', __added.filter(n => n.textContent.startsWith('Your sign-in ended.')).length === 1);
__up = true; await tick(200);
check('a server that answers again does not claim the sign-in is back', shown() === '● signed out');

// An account store that cannot answer (503) is not a sign-in that ended.
signInGone = false; signedIn = true; __added.length = 0; __status = 503; __meStatus = 503; __me = {error:'could not check your sign-in'};
try{ await api('/v1/sessions'); }catch{}
__up = false; try{ await api('/v1/sessions'); }catch{} await tick();
__up = true; await tick(200);
check('a 503 does not say the sign-in ended', !__added.some(n => n.textContent.startsWith('Your sign-in ended.')) && shown() === '● connected');
__meStatus = 200;

// A restart ends every sign-in: once the server is back, the probe asks who is signed in.
signInGone = false; signedIn = true; __status = 200; __added.length = 0; __me = {authenticated:false};
__up = false; try{ await api('/v1/sessions'); }catch{} await tick();
__up = true; await tick(200);
check('a server back from a restart with the sign-in gone says so', shown() === '● signed out' &&
  __added.some(n => n.textContent.startsWith('Your sign-in ended.')));

// Once signed out, the idle check does not keep asking.
__fetches = 0; es = null; connState = true; connIdle(); await tick();
check('a signed-out page is not probed on the timer', signInGone && __fetches === 0);

// A member dropped from require_group gets a 403 with the reason: signed out, told why.
signInGone = false; signedIn = true; es = null; __up = true; __added.length = 0; __status = 403;
__body = {error:'forbidden', reason:'your account is not in the eng group', refused:true};
let refusedErr = null; try{ await api('/v1/sessions'); }catch(e){ refusedErr = e; }
const refusedNote = __added.find(n => n.textContent.startsWith('Your sign-in ended: your account is not in the eng group'));
check('a refused member is signed out with the reason', !!refusedNote && shown() === '● signed out');
check('and the request names the reason, not "forbidden"', !!refusedErr && refusedErr.message === 'your account is not in the eng group');
check('and the way back carries the reason', !!refusedNote && refusedNote.childNodes.some(c => c.href === '/?refused=your%20account%20is%20not%20in%20the%20eng%20group&return=/ide'));
// Any other 403 is a refused action, not a sign-in that ended.
signInGone = false; __added.length = 0; __body = {error:'denied by rule'};
try{ await api('/v1/sessions'); }catch{}
check('a rule\'s 403 does not sign out', !signInGone && __added.length === 0);
__status = 200; __body = null;

// A live stream refused for a signed-out person is not retried every second:
// the retry asks first, learns the sign-in ended, and stops.
signInGone = false; signedIn = true; es = null; __up = true; __added.length = 0; live = false; bgLive = true;
bgTasks.set('t1', {name:'sleep 60', state:'running'}); __bgDrawn = 0;
connect('s1'); last().onopen(); const opened = __streams.length;
__status = 401; last().onerror(); await tick(200);
check('a stream dropped by a sign-out is not reopened', __streams.length === opened && signInGone && shown() === '● signed out');
check('and the page stops listing background work it can no longer follow', !bgLive && bgTasks.size === 0 && __bgDrawn > 0);
// Every panel shows a 401's message: it says what to do, not "unauthorized".
__body = {error:'unauthorized'}; let e401 = null; try{ await api('/v1/sessions/s1/tree?path='); }catch(e){ e401 = e; }
check('a 401 reads as a sign-in that ended', !!e401 && e401.message === 'your sign-in ended; sign in again');
// A stream that drops while still signed in is reopened as before.
signInGone = false; __status = 200; __body = null; __me = {authenticated:true}; bgLive = true; connect('s1'); last().onopen();
const before = __streams.length; last().onerror(); await tick(200);
check('a stream dropped while signed in is reopened', __streams.length === before + 1);
bgLive = false;

// A listed session still starting on another server answers 404 for a moment: the
// stream is asked again three times, after 0.5, 1 and 1.5 s, then the page says so and stops.
signInGone = false; signedIn = true; es = null; __up = true; __me = {authenticated:true}; live = false; bgLive = false;
current = 's9'; __added.length = 0; __waits.length = 0; __status = 404; __body = {error:'session not found'};
const before404 = __streams.length;
connect('s9');
for(let i = 0; i < 6; i++){ const st = last(); if(st.readyState !== 2) st.onerror(); await tick(80); }
const starting = n => n.textContent.includes('still starting on another server');
check('a stream refused with 404 is asked again three times, then no more', __streams.length - before404 === 4);
check('with a short backoff', [500, 1000, 1500].every(ms => __waits.includes(ms)));
check('then the page says the session is still starting, once', __added.filter(starting).length === 1);
// One 404, then the session answers: it opens with nothing said.
__added.length = 0; startTries.clear(); startNoted.clear(); __status = 404;
connect('s9'); last().onerror(); __status = 200; __body = null; await tick(80);
last().onopen();
check('a session that answers on a retry opens with no notice', shown() === '● connected' && !__added.some(starting) && startTries.size === 0);
// A stream that opened and then dropped is not taken for a starting session.
__status = 404; __added.length = 0; const openedN = __streams.length; last().onerror(); await tick(80);
check('a stream that had opened is not retried as a starting session', __streams.length === openedN && !__added.some(starting));
current = 's1'; __status = 200; __body = null;

if(!ok) process.exit(1);
