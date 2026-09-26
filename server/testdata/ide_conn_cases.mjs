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

if(!ok) process.exit(1);
