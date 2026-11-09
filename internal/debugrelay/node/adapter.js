// Abhed's Debug Adapter Protocol server for Node, run inside the session's
// sandbox: DAP on stdin and stdout, the inspector of the program it starts
// through the shim's pipe, never a port.
'use strict';
const cp = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const { fileURLToPath } = require('node:url');

const SHIM = __ABHED_SHIM__;
const HELPER = 'abhed-debug';
// ENTRY matches the URL of every script but those in node_modules.
const ENTRY = '^(?:file://)?/(?!.*/node_modules/)';

// ---- DAP over stdio

let seq = 0;
function send(msg) {
  msg.seq = ++seq;
  const body = Buffer.from(JSON.stringify(msg), 'utf8');
  process.stdout.write('Content-Length: ' + body.length + '\r\n\r\n');
  process.stdout.write(body);
}
function respond(req, body) {
  send({ type: 'response', request_seq: req.seq, command: req.command, success: true, body: body || {} });
}
function fail(req, message) {
  send({ type: 'response', request_seq: req.seq, command: req.command, success: false, message: String(message) });
}
function event(name, body) {
  send({ type: 'event', event: name, body: body || {} });
}

let inbuf = Buffer.alloc(0);
process.stdin.on('data', (d) => {
  inbuf = Buffer.concat([inbuf, d]);
  for (;;) {
    const end = inbuf.indexOf('\r\n\r\n');
    if (end < 0) return;
    const m = /Content-Length:\s*(\d+)/i.exec(inbuf.subarray(0, end).toString('latin1'));
    if (!m) process.exit(2);
    const n = Number(m[1]);
    if (inbuf.length < end + 4 + n) return;
    const body = inbuf.subarray(end + 4, end + 4 + n).toString('utf8');
    inbuf = inbuf.subarray(end + 4 + n);
    let req;
    try { req = JSON.parse(body); } catch { continue; }
    if (req && req.type === 'request') onRequest(req);
  }
});
process.stdin.on('end', () => shutdown(0));

// ---- source maps

const B64 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';
const B64I = new Map([...B64].map((c, i) => [c, i]));

// decodeMappings is a map's mappings as [genLine][] of [genCol, src, line, col].
function decodeMappings(s) {
  const lines = [[]];
  let col = 0, src = 0, line = 0, scol = 0;
  let i = 0;
  const vlq = () => {
    let v = 0, shift = 0, c;
    do {
      c = B64I.get(s[i++]);
      if (c === undefined) throw new Error('bad mappings');
      v += (c & 31) << shift;
      shift += 5;
    } while (c & 32);
    return v & 1 ? -(v >>> 1) : v >>> 1;
  };
  while (i < s.length) {
    const ch = s[i];
    if (ch === ';') { lines.push([]); col = 0; i++; continue; }
    if (ch === ',') { i++; continue; }
    col += vlq();
    if (i < s.length && s[i] !== ',' && s[i] !== ';') {
      src += vlq(); line += vlq(); scol += vlq();
      if (i < s.length && s[i] !== ',' && s[i] !== ';') vlq();
      lines[lines.length - 1].push([col, src, line, scol]);
    }
  }
  return lines;
}

function scriptPath(url) {
  if (!url) return '';
  if (url.startsWith('file://')) {
    try { return fileURLToPath(url); } catch { return ''; }
  }
  return path.isAbsolute(url) ? url : '';
}

// loadMap reads a script's source map, inline or beside it, or null.
function loadMap(script) {
  const ref = script.sourceMapURL;
  if (!ref) return null;
  let text;
  try {
    if (ref.startsWith('data:')) {
      const comma = ref.indexOf(',');
      const meta = ref.slice(5, comma);
      const data = ref.slice(comma + 1);
      text = meta.endsWith(';base64') ? Buffer.from(data, 'base64').toString('utf8') : decodeURIComponent(data);
    } else {
      const own = scriptPath(script.url);
      const p = ref.startsWith('file://') ? fileURLToPath(ref) : path.resolve(path.dirname(own || '/'), ref);
      text = fs.readFileSync(p, 'utf8');
    }
    const raw = JSON.parse(text);
    if (raw.version !== 3 || !Array.isArray(raw.sources) || typeof raw.mappings !== 'string') return null;
    const base = path.dirname(scriptPath(script.url) || '/');
    const sources = raw.sources.map((s) => {
      if (typeof s !== 'string') return '';
      const full = (raw.sourceRoot || '') + s;
      if (full.startsWith('file://')) {
        try { return fileURLToPath(full); } catch { return ''; }
      }
      if (/^[a-z][a-z0-9+.-]*:/i.test(full)) return '';
      return path.resolve(base, full);
    });
    return { sources, lines: decodeMappings(raw.mappings) };
  } catch {
    return null;
  }
}

// generated is where an original line (0-based) of source src runs, trying
// the lines after it as an editor moves a breakpoint, or null.
function generated(map, src, line) {
  for (let want = line; want < line + 20; want++) {
    let best = null;
    map.lines.forEach((segs, g) => {
      for (const [col, s, l, c] of segs) {
        if (s === src && l === want && (!best || g < best.lineNumber || g === best.lineNumber && col < best.columnNumber || best.col > c)) {
          best = { lineNumber: g, columnNumber: col, col: c };
        }
      }
    });
    if (best) return { lineNumber: best.lineNumber, columnNumber: best.columnNumber };
  }
  return null;
}

// original is a generated position's source and line (0-based), or null.
function original(map, line, col) {
  const segs = map.lines[line];
  if (!segs || !segs.length) return null;
  let hit = null;
  for (const seg of segs) {
    if (seg[0] <= col) hit = seg;
    else break;
  }
  hit = hit || segs[0];
  const p = map.sources[hit[1]];
  return p ? { path: p, line: hit[2], column: hit[3] } : null;
}

// ---- targets: the main thread, and each worker thread

let child = null;
let channel = null;
let configured = false;
let went = false;
let ended = false;
let pauseOnExceptions = 'none';
const breakpoints = new Map(); // path -> [{line, condition}]
const targets = new Map(); // thread id -> Target
let nextThread = 1;

class Target {
  constructor(name, post) {
    this.id = nextThread++;
    this.name = name;
    this.post = post;
    this.calls = new Map();
    this.nextCall = 1;
    this.scripts = new Map();
    this.maps = new Map();
    this.paused = null;
    this.why = '';
    this.ready = false;
    this.set = new Map(); // path -> [inspector breakpoint ids]
    this.queue = Promise.resolve();
    this.entry = '';
    this.applied = new Set(); // scriptId + '\n' + path, set there
    this.at = new Set(); // where breakpoints were set: scriptId:line:column
    targets.set(this.id, this);
  }
  // serial runs fn after every breakpoint change queued before it.
  serial(fn) {
    const next = this.queue.then(fn, fn);
    this.queue = next.catch(() => {});
    return next;
  }
  call(method, params) {
    const id = this.nextCall++;
    return new Promise((resolve, reject) => {
      this.calls.set(id, { resolve, reject });
      this.post({ id, method, params: params || {} });
    });
  }
  answer(m) {
    const c = this.calls.get(m.id);
    if (!c) return;
    this.calls.delete(m.id);
    if (m.error) c.reject(new Error(m.error.message || 'inspector error'));
    else c.resolve(m.result || {});
  }
  map(scriptId) {
    if (!this.maps.has(scriptId)) {
      const s = this.scripts.get(scriptId);
      this.maps.set(scriptId, s ? loadMap(s) : null);
    }
    return this.maps.get(scriptId);
  }
  async setup() {
    await this.call('Debugger.enable', { maxScriptsCacheSize: 0 });
    await this.call('Debugger.setPauseOnExceptions', { state: pauseOnExceptions });
    this.ready = true;
    for (const p of breakpoints.keys()) this.serial(() => this.apply(p));
    await this.queue;
  }
  // entryPoints stops each of the program's own scripts (not node_modules)
  // before it runs, while there are breakpoints: its source map is read
  // then and its breakpoints set where they fall, as a script compiled in
  // memory (tsx, ts-node, a test runner) has none on disk beforehand.
  async entryPoints() {
    const want = [...breakpoints.values()].some((b) => b.length);
    if (want && !this.entry) {
      const r = await this.call('Debugger.setBreakpointByUrl', { urlRegex: ENTRY, lineNumber: 0, columnNumber: 0 }).catch(() => null);
      if (r) this.entry = r.breakpointId;
    } else if (!want && this.entry) {
      await this.call('Debugger.removeBreakpoint', { breakpointId: this.entry }).catch(() => {});
      this.entry = '';
    }
  }
  // apply sets path's breakpoints here afresh, in every script loaded.
  async apply(p) {
    for (const id of this.set.get(p) || []) {
      await this.call('Debugger.removeBreakpoint', { breakpointId: id }).catch(() => {});
    }
    this.set.set(p, []);
    for (const k of this.applied) if (k.endsWith('\n' + p)) this.applied.delete(k);
    await this.entryPoints();
    for (const id of this.scripts.keys()) await this.applyScript(id, p);
  }
  // applyScript sets the breakpoints of path p (every path, for '') that
  // fall in one script, through its source map or as it is, once.
  async applyScript(scriptId, only) {
    const s = this.scripts.get(scriptId);
    if (!s || s.skip) return;
    const map = this.map(scriptId);
    for (const [p, want] of breakpoints) {
      if (only && p !== only || this.applied.has(scriptId + '\n' + p)) continue;
      this.applied.add(scriptId + '\n' + p);
      for (const b of want) {
        let loc = null;
        if (map) {
          const src = map.sources.indexOf(p);
          if (src >= 0) loc = generated(map, src, b.line - 1);
        } else if (s.path === p) {
          loc = { lineNumber: b.line - 1, columnNumber: 0 };
        }
        if (!loc) continue;
        const r = await this.call('Debugger.setBreakpoint', { location: { scriptId, ...loc }, condition: b.condition || '' }).catch(() => null);
        if (r) {
          if (!this.set.has(p)) this.set.set(p, []);
          this.set.get(p).push(r.breakpointId);
          const at = r.actualLocation || { scriptId, ...loc };
          this.at.add(at.scriptId + ':' + at.lineNumber + ':' + (at.columnNumber || 0));
        }
      }
    }
  }
  async notify(method, params) {
    switch (method) {
      case 'Debugger.scriptParsed': {
        const p = scriptPath(params.url);
        this.scripts.set(params.scriptId, { url: params.url, sourceMapURL: params.sourceMapURL || '', path: p,
          skip: !p || p.includes(path.sep + 'node_modules' + path.sep) });
        return;
      }
      case 'Debugger.paused': {
        if (params.reason === 'Break on start') {
          await this.call('Debugger.resume').catch(() => {});
          return;
        }
        const top = params.callFrames[0];
        const hits = params.hitBreakpoints || [];
        if (this.entry && hits.includes(this.entry)) {
          await this.serial(() => this.applyScript(top.location.scriptId, ''));
          const here = top.location.scriptId + ':' + top.location.lineNumber + ':' + (top.location.columnNumber || 0);
          if (hits.length === 1 && !this.at.has(here)) {
            await this.call('Debugger.resume').catch(() => {});
            return;
          }
          params = { ...params, hitBreakpoints: hits.filter((h) => h !== this.entry).concat(['entry']) };
        }
        this.paused = params;
        let reason = this.why || 'pause';
        if (params.hitBreakpoints && params.hitBreakpoints.length) reason = 'breakpoint';
        else if (params.reason === 'exception' || params.reason === 'promiseRejection') reason = 'exception';
        else if (params.reason === 'debugCommand' || params.reason === 'other') reason = this.why || 'breakpoint';
        this.why = '';
        const body = { reason, threadId: this.id, allThreadsStopped: false };
        if (reason === 'exception' && params.data) body.text = params.data.description || '';
        event('stopped', body);
        return;
      }
      case 'Debugger.resumed':
        if (this.paused) {
          this.paused = null;
          forget(this);
          event('continued', { threadId: this.id, allThreadsContinued: false });
        }
        return;
      case 'NodeWorker.attachedToWorker':
        return attachWorker(this, params);
      case 'NodeWorker.detachedFromWorker':
        for (const t of targets.values()) {
          if (t.session === params.sessionId) {
            targets.delete(t.id);
            event('thread', { reason: 'exited', threadId: t.id });
          }
        }
        return;
      case 'NodeWorker.receivedMessageFromWorker': {
        for (const t of targets.values()) {
          if (t.session === params.sessionId) {
            let m;
            try { m = JSON.parse(params.message); } catch { return; }
            if (m.id !== undefined) t.answer(m);
            else t.notify(m.method, m.params || {}).catch(() => {});
          }
        }
        return;
      }
    }
  }
}

async function attachWorker(parent, params) {
  const { sessionId, workerInfo } = params;
  if (String(workerInfo.title || '').includes(HELPER)) {
    await parent.call('NodeWorker.detach', { sessionId }).catch(() => {});
    return;
  }
  const t = new Target(workerInfo.title || 'worker', (msg) => {
    parent.call('NodeWorker.sendMessageToWorker', { sessionId, message: JSON.stringify(msg) }).catch(() => {});
  });
  t.session = sessionId;
  event('thread', { reason: 'started', threadId: t.id });
  try {
    await t.setup();
  } catch { /* a worker that ended while it was set up */ }
  await t.call('Runtime.runIfWaitingForDebugger').catch(() => {});
}

// ---- handles for a stop: frames, scopes and objects

let nextHandle = 1;
const handles = new Map();
function handle(target, v) {
  const h = nextHandle++;
  handles.set(h, { target, ...v });
  return h;
}
function forget(target) {
  for (const [h, v] of handles) if (v.target === target) handles.delete(h);
}

function render(o) {
  if (!o) return 'undefined';
  switch (o.type) {
    case 'string': return JSON.stringify(o.value);
    case 'undefined': return 'undefined';
    case 'number': case 'boolean': case 'bigint': return o.description !== undefined ? o.description : String(o.value);
    case 'symbol': return o.description || 'Symbol()';
    case 'function': return (o.description || 'function').split('\n')[0].slice(0, 200);
    case 'object': return o.subtype === 'null' ? 'null' : (o.description || o.className || 'Object');
  }
  return o.description || String(o.value);
}

function variable(target, name, o) {
  const ref = o && o.objectId && (o.type === 'object' && o.subtype !== 'null' || o.type === 'function') ? handle(target, { objectId: o.objectId }) : 0;
  return { name, value: render(o), type: o ? (o.subtype || o.type) : 'undefined', variablesReference: ref };
}

function frameSource(target, frame) {
  const s = target.scripts.get(frame.location.scriptId);
  const map = target.map(frame.location.scriptId);
  if (map) {
    const o = original(map, frame.location.lineNumber, frame.location.columnNumber || 0);
    if (o) return { source: { name: path.basename(o.path), path: o.path }, line: o.line + 1, column: o.column + 1 };
  }
  const line = frame.location.lineNumber + 1;
  const column = (frame.location.columnNumber || 0) + 1;
  if (s && s.path) {
    const src = { name: path.basename(s.path), path: s.path };
    if (s.skip) src.presentationHint = 'deemphasize';
    return { source: src, line, column };
  }
  return { source: { name: (s && s.url) || 'unknown', presentationHint: 'deemphasize' }, line, column };
}

const SCOPES = { local: 'Local', closure: 'Closure', block: 'Block', global: 'Global', script: 'Script', module: 'Module',
  catch: 'Catch', with: 'With', eval: 'Eval', wasmExpressionStack: 'Stack' };

function threadOf(args) {
  const t = targets.get(args && args.threadId);
  if (!t) throw new Error('no such thread');
  return t;
}

// ---- requests

async function onRequest(req) {
  const a = req.arguments || {};
  try {
    switch (req.command) {
      case 'initialize':
        return respond(req, {
          supportsConfigurationDoneRequest: true,
          supportsConditionalBreakpoints: true,
          supportsEvaluateForHovers: true,
          supportsTerminateRequest: true,
          exceptionBreakpointFilters: [
            { filter: 'uncaught', label: 'Uncaught Exceptions', default: false },
            { filter: 'all', label: 'All Exceptions', default: false },
          ],
        });
      case 'launch':
        return launch(req, a);
      case 'setBreakpoints': {
        const p = a.source && a.source.path ? path.resolve(a.source.path) : '';
        const want = (a.breakpoints || []).map((b) => ({ line: b.line, condition: b.condition }));
        if (p) breakpoints.set(p, want);
        for (const t of targets.values()) if (t.ready) await t.serial(() => t.apply(p));
        return respond(req, { breakpoints: want.map((b) => ({ verified: !!p, line: b.line })) });
      }
      case 'setExceptionBreakpoints': {
        const f = a.filters || [];
        pauseOnExceptions = f.includes('all') ? 'all' : f.includes('uncaught') ? 'uncaught' : 'none';
        for (const t of targets.values()) await t.call('Debugger.setPauseOnExceptions', { state: pauseOnExceptions }).catch(() => {});
        return respond(req);
      }
      case 'configurationDone':
        configured = true;
        respond(req);
        return maybeGo();
      case 'threads':
        return respond(req, { threads: [...targets.values()].map((t) => ({ id: t.id, name: t.name })) });
      case 'stackTrace': {
        const t = threadOf(a);
        if (!t.paused) return respond(req, { stackFrames: [], totalFrames: 0 });
        const all = t.paused.callFrames;
        const start = a.startFrame || 0;
        const frames = all.slice(start, a.levels ? start + a.levels : undefined).map((f) => ({
          id: handle(t, { frame: f }), name: f.functionName || '<anonymous>', ...frameSource(t, f),
        }));
        return respond(req, { stackFrames: frames, totalFrames: all.length });
      }
      case 'scopes': {
        const h = handles.get(a.frameId);
        if (!h || !h.frame) throw new Error('no such frame');
        return respond(req, { scopes: h.frame.scopeChain.map((s) => ({
          name: SCOPES[s.type] || s.type, expensive: s.type === 'global',
          variablesReference: handle(h.target, { objectId: s.object.objectId }),
        })) });
      }
      case 'variables': {
        const h = handles.get(a.variablesReference);
        if (!h || !h.objectId) throw new Error('no such variable');
        const r = await h.target.call('Runtime.getProperties', { objectId: h.objectId, ownProperties: true, generatePreview: false });
        const vars = [];
        for (const p of r.result || []) {
          if (vars.length >= 1000) break;
          if (p.value) vars.push(variable(h.target, p.name, p.value));
          else if (p.get) vars.push({ name: p.name, value: '(getter)', variablesReference: 0 });
        }
        return respond(req, { variables: vars });
      }
      case 'evaluate': {
        const h = a.frameId ? handles.get(a.frameId) : null;
        let r;
        if (h && h.frame) {
          r = await h.target.call('Debugger.evaluateOnCallFrame', { callFrameId: h.frame.callFrameId, expression: a.expression,
            throwOnSideEffect: a.context === 'hover' });
        } else {
          const t = targets.get(1);
          if (!t) throw new Error('the program is not running');
          r = await t.call('Runtime.evaluate', { expression: a.expression, throwOnSideEffect: a.context === 'hover' });
        }
        if (r.exceptionDetails) {
          throw new Error(r.result && r.result.description ? r.result.description.split('\n')[0] : r.exceptionDetails.text);
        }
        const v = variable(h ? h.target : targets.get(1), '', r.result);
        return respond(req, { result: v.value, type: v.type, variablesReference: v.variablesReference });
      }
      case 'continue': {
        const t = threadOf(a);
        await t.call('Debugger.resume');
        return respond(req, { allThreadsContinued: false });
      }
      case 'next': case 'stepIn': case 'stepOut': {
        const t = threadOf(a);
        t.why = 'step';
        await t.call({ next: 'Debugger.stepOver', stepIn: 'Debugger.stepInto', stepOut: 'Debugger.stepOut' }[req.command]);
        return respond(req);
      }
      case 'pause': {
        const t = threadOf(a);
        t.why = 'pause';
        await t.call('Debugger.pause');
        return respond(req);
      }
      case 'terminate':
        respond(req);
        return stopProgram();
      case 'disconnect':
        stopProgram();
        return respond(req);
      default:
        return fail(req, 'not supported: ' + req.command);
    }
  } catch (e) {
    return fail(req, e && e.message ? e.message : e);
  }
}

function launch(req, a) {
  if (child) return fail(req, 'already launched');
  const [maj, min] = process.versions.node.split('.').map(Number);
  if (maj < 20 || maj === 20 && min < 6) {
    return fail(req, 'debugging needs Node 20.6 or newer here; this is Node ' + process.versions.node);
  }
  if (!Array.isArray(a.argv) || !a.argv.length) return fail(req, 'nothing to run');
  const argv = a.argv.slice();
  for (const r of a.resolve || []) {
    const found = inModules(a.cwd, a.root, r.rel);
    if (!found) {
      return fail(req, r.name + ' is not installed in this workspace: run npm install, or add it with npm install -D ' + r.name);
    }
    if (r.index >= 0) argv[r.index] = found;
  }
  // Node 22 has the test runner's isolation flag under its older name.
  const iso = argv.indexOf('--test-isolation=none');
  if (iso >= 0 && cp.spawnSync(process.execPath, ['--test-isolation=none', '-e', '0']).status !== 0) {
    argv[iso] = '--experimental-test-isolation=none';
  }
  const env = { ...process.env, ...(a.env || {}), ABHED_NODE_DEBUG: '3' };
  try {
    child = cp.spawn(process.execPath, ['--import', 'data:text/javascript,' + encodeURIComponent(SHIM), ...argv],
      { cwd: a.cwd, env, stdio: ['ignore', 'pipe', 'pipe', 'pipe'] });
  } catch (e) {
    return fail(req, 'the program could not start: ' + e.message);
  }
  child.on('error', (e) => {
    event('output', { category: 'stderr', output: 'the program could not start: ' + e.message + '\n' });
  });
  for (const [stream, category] of [[child.stdout, 'stdout'], [child.stderr, 'stderr']]) {
    stream.setEncoding('utf8');
    stream.on('data', (d) => event('output', { category, output: d }));
  }
  child.on('exit', (code, signal) => {
    if (ended) return;
    ended = true;
    event('exited', { exitCode: code === null ? 128 + (signal === 'SIGKILL' ? 9 : 15) : code });
    event('terminated');
  });
  channel = child.stdio[3];
  channel.setEncoding('utf8');
  let rest = '';
  let main = null;
  channel.on('data', (d) => {
    rest += d;
    for (let i = rest.indexOf('\n'); i >= 0; i = rest.indexOf('\n')) {
      const line = rest.slice(0, i);
      rest = rest.slice(i + 1);
      let m;
      try { m = JSON.parse(line); } catch { continue; }
      if (m.hello && !main) {
        main = new Target('main', (msg) => channel.write(JSON.stringify({ c: msg }) + '\n'));
        event('thread', { reason: 'started', threadId: main.id });
        main.setup()
          .then(() => main.call('NodeWorker.enable', { waitForDebuggerOnStart: true }))
          .then(maybeGo, (e) => {
            event('output', { category: 'stderr', output: 'abhed-debug: ' + e.message + '\n' });
            maybeGo();
          });
      } else if (m.r && main) {
        main.answer(m.r);
      } else if (m.e && main) {
        main.notify(m.e.method, m.e.params || {}).catch(() => {});
      }
    }
  });
  channel.on('error', () => {});
  respond(req);
  event('initialized');
}

// inModules is rel in the nearest node_modules from dir up to root, as
// node looks, or ''.
function inModules(dir, root, rel) {
  for (let d = path.resolve(dir); ; d = path.dirname(d)) {
    const p = path.join(d, 'node_modules', rel);
    if (fs.existsSync(p)) return p;
    if (d === path.resolve(root) || d === path.dirname(d)) return '';
  }
}

async function maybeGo() {
  const main = targets.get(1);
  if (went || !configured || !main || !main.ready || !channel) return;
  went = true;
  await main.queue;
  channel.write(JSON.stringify({ go: true }) + '\n');
}

function stopProgram() {
  if (child && child.exitCode === null && child.signalCode === null) {
    child.kill('SIGTERM');
    const c = child;
    setTimeout(() => { if (c.exitCode === null && c.signalCode === null) c.kill('SIGKILL'); }, 2000).unref();
  }
}

function shutdown(code) {
  if (!child || child.exitCode !== null || child.signalCode !== null) process.exit(code);
  stopProgram();
  child.once('exit', () => process.exit(code));
  setTimeout(() => { child.kill('SIGKILL'); process.exit(code); }, 3000);
}
