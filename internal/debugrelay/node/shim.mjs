// Loaded with --import into the program the Node adapter starts. A worker
// thread drives the main thread's inspector in process and carries its
// messages over the pipe the adapter left on fd 3, so nothing listens on a
// port. The program waits, here, until the adapter has set its breakpoints.
import { Worker, isMainThread } from 'node:worker_threads';

const fd = process.env.ABHED_NODE_DEBUG;
delete process.env.ABHED_NODE_DEBUG;

const helper = `
const { parentPort } = require('node:worker_threads');
const inspector = require('node:inspector');
const net = require('node:net');
const ch = new net.Socket({ fd: 3, readable: true, writable: true });
const gone = () => process.kill(process.pid, 'SIGKILL');
ch.on('error', gone);
ch.on('close', gone);
const out = (m) => ch.write(JSON.stringify(m) + '\\n');
const s = new inspector.Session();
s.connectToMainThread();
s.on('inspectorNotification', (m) => out({ e: { method: m.method, params: m.params } }));
let rest = '';
ch.setEncoding('utf8');
ch.on('data', (d) => {
  rest += d;
  for (let i = rest.indexOf('\\n'); i >= 0; i = rest.indexOf('\\n')) {
    const line = rest.slice(0, i);
    rest = rest.slice(i + 1);
    let m;
    try { m = JSON.parse(line); } catch { continue; }
    if (m.go) { parentPort.postMessage('go'); continue; }
    if (m.c) {
      const { id, method, params } = m.c;
      try {
        s.post(method, params || {}, (err, result) => out({ r: err ? { id, error: { message: String(err.message || err) } } : { id, result } }));
      } catch (err) {
        out({ r: { id, error: { message: String(err.message || err) } } });
      }
    }
  }
});
out({ hello: { pid: process.pid } });
`;

// Only the process the adapter started, on its main thread: its worker
// threads and child processes inherit --import, not the variable.
if (isMainThread && fd === '3') {
  const w = new Worker(helper, { eval: true, name: 'abhed-debug', execArgv: [] });
  w.on('error', (e) => {
    process._rawDebug('abhed-debug: ' + (e && e.message));
    process.kill(process.pid, 'SIGKILL');
  });
  await new Promise((resolve) => w.once('message', resolve));
  w.unref();
}
