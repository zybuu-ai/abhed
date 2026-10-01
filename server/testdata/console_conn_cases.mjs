// Assertions driven against the console's real connect(), spliced in above
// this file's contents by console_render_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const drop = () => { const e = __streams.at(-1); e.onerror(); };

// A session with background work owed keeps its stream: a drop reconnects.
current = 's1'; live = false; bgLive = true; __streams.length = 0;
connect('s1'); drop();
check('a stream dropped while background work is owed reconnects', __streams.length === 2 && __streams[1].url === '/v1/sessions/s1/events');

// A live run's stream reconnects too.
live = true; bgLive = false; __streams.length = 0;
connect('s1'); drop();
check('a live run\'s stream reconnects', __streams.length === 2);

// A finished session's stream closes for good.
live = false; bgLive = false; __streams.length = 0;
connect('s1'); drop();
check('a finished session\'s stream is not reopened', __streams.length === 1);

// Nor does one reconnect for a session no longer open.
live = true; bgLive = true; __streams.length = 0;
connect('s1'); current = 's2'; drop();
check('a stream for another session is not reopened', __streams.length === 1);

if(!ok) process.exit(1);
