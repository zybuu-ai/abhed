let failed = 0;
const check = (ok, what) => { if(!ok){ failed++; console.log('FAIL ' + what); } else console.log('ok   ' + what); };
const tick = () => new Promise(r => globalThis.setTimeout(r, 40));
const muxES = () => __es[__es.length - 1];
const posts = () => __fetches.filter(f => f.method === 'POST');

const a = new StreamSource('/v1/sessions/s1/events?after=3'), b = new StreamSource('/v1/sessions/s1/pty/u1');
check(__es.length === 1 && muxES().url === '/v1/streams', 'one shared stream for every StreamSource');
check(posts().length === 0, 'nothing is asked for before the shared stream is named');
muxES().emit('mux', {id:'m1'}); await tick();
check(posts().length === 2 && posts()[0].body.path === '/v1/sessions/s1/events?after=3' && posts()[1].body.path === '/v1/sessions/s1/pty/u1', 'each stream is asked for by its own route');
const chA = posts()[0].url.split('/').pop(), chB = posts()[1].url.split('/').pop();
let opened = 0, msgs = [], outs = [], errs = 0;
a.onopen = () => opened++; a.onmessage = e => msgs.push(e.data); a.onerror = () => errs++;
b.addEventListener('out', e => outs.push(e.data));
muxES().emit('c', {ch:chA, t:'open'}); muxES().emit('c', {ch:chA, t:'ev', data:'{"seq":4}', id:'4', has_id:true});
muxES().emit('c', {ch:chB, t:'open'}); muxES().emit('c', {ch:chB, t:'ev', event:'out', data:'aGk='});
muxES().emit('c', {ch:'c999', t:'ev', data:'stray'});
check(a.readyState === 1 && opened === 1 && msgs.join() === '{"seq":4}', 'an unnamed event reaches onmessage');
check(outs.join() === 'aGk=' && !msgs.includes('stray'), 'a named event reaches its listener; another channel\'s does not');

// The server ends a stream: as EventSource, it is asked for again from the last id.
__fetches.length = 0;
muxES().emit('c', {ch:chA, t:'end'});
check(a.readyState === 0 && errs === 1, 'an ended stream reconnects');
await tick();
check(posts().length === 1 && posts()[0].body.last_event_id === '4' && posts()[0].url.split('/').pop() !== chA, 'again, as a new channel, from the last id');
const chA2 = posts()[0].url.split('/').pop();
muxES().emit('c', {ch:chA, t:'ev', data:'old'});
check(!msgs.includes('old'), 'the ended channel\'s frames are not taken');
muxES().emit('c', {ch:chA2, t:'open'});
check(a.readyState === 1 && opened === 2, 'and opens again');

// A refusal closes it, for good.
let bErr = 0; b.onerror = () => bErr++;
__fetches.length = 0;
muxES().emit('c', {ch:chB, t:'fail', status:404, message:'session not found'});
await tick();
check(b.readyState === 2 && bErr === 1 && posts().length === 0, 'a refused stream closes and is not asked for again');

// The shared stream drops: each is asked for again once it is back.
__fetches.length = 0;
muxES().readyState = 0; muxES().onerror();
check(a.readyState === 0 && errs === 2, 'a dropped shared stream drops each stream');
muxES().readyState = 1; muxES().emit('mux', {id:'m2'}); await tick();
check(posts().length >= 1 && posts().every(p => p.url.startsWith('/v1/streams/m2/')), 'each is asked for on the new one');

// close() tells the server.
__fetches.length = 0;
a.close(); await tick();
check(a.readyState === 2 && __fetches.some(f => f.method === 'DELETE' && f.url.startsWith('/v1/streams/m2/')), 'close ends the channel on the server');

// A shared stream the server no longer has is opened again.
__status = 404; const c = new StreamSource('/v1/sessions/s1/lsp/l1'); await tick();
check(__es.length === 2 && c.readyState === 0, 'a lost shared stream is opened again');
__status = 202;
// Signed out: the shared stream is refused, and so is every stream on it.
let cErr = 0; c.onerror = () => cErr++;
muxES().readyState = 2; muxES().onerror();
check(c.readyState === 2 && cErr === 1, 'a refused shared stream closes each stream');
if(failed) process.exit(1);
