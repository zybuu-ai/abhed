package server

import "testing"

// filesHarness is the page state the downloads, title, delete and notification
// code runs against: elements by id, a route that records what it was asked,
// and a browser's clipboard, confirm, storage and Notification.
const filesHarness = `import { El } from './dom.mjs';
globalThis.__root = new El('div');
El.prototype.addEventListener = function(type, f){ (this.on = this.on || {})[type] = f; };
El.prototype.focus = function(){ globalThis.__focused = this; };
El.prototype.select = () => {};
El.prototype.removeChild = function(c){ this.childNodes.splice(this.childNodes.indexOf(c), 1); c.parentNode = null; return c; };
El.prototype.remove = function(){ const p = this.parentNode; if(p){ p.childNodes.splice(p.childNodes.indexOf(this), 1); this.parentNode = null; } };
El.prototype.click = function(){ __clicked.push({href:this.href, download:this.download}); };
Object.defineProperty(El.prototype, 'firstChild', {get(){ return this.childNodes[0] || null; }});
const ids = {}, $ = id => ids[id] || (ids[id] = new El('div'));
document.body = new El('body');
let current = 's1', sessionList = [];
globalThis.__files = []; globalThis.__heads = []; globalThis.__clicked = []; globalThis.__headStatus = 200;
globalThis.__posts = []; globalThis.__deletes = []; globalThis.__notes = []; globalThis.__asked = []; globalThis.__confirm = false;
globalThis.__titleError = ''; globalThis.__deleteStatus = 0; globalThis.__reset = 0; globalThis.__clipboard = '';
const api = async (url, opts) => {
  const method = (opts && opts.method) || 'GET';
  if(method === 'GET' && url.endsWith('/files')) return __files;
  if(method === 'POST' && url.endsWith('/title')){ const body = JSON.parse(opts.body); __posts.push({url, body});
    if(__titleError){ const e = new Error(__titleError); e.status = 409; throw e; } return {title: body.title}; }
  if(method === 'DELETE'){ if(__deleteStatus){ const e = new Error('not implemented'); e.status = __deleteStatus; throw e; } __deletes.push(url); return null; }
  throw new Error('unexpected ' + method + ' ' + url);
};
globalThis.fetch = async (url, opts) => { __heads.push(url); return {status: __headStatus, ok: __headStatus < 300}; };
const connLost = () => {}, signInEnded = () => {}, loadSessions = () => {}, add = () => {}, resetSession = () => { __reset++; };
const sessionsNote = t => __notes.push(t), shellsKey = id => 'k.' + id;
globalThis.sessionStorage = {removeItem(){}};
globalThis.confirm = q => { __asked.push(q); return __confirm; };
Object.defineProperty(globalThis, 'navigator', {value: {clipboard: {writeText: async t => { __clipboard = t; }}}, configurable: true});
globalThis.__store = {}; const store = {get: (k, d) => k in __store ? __store[k] : d, set: (k, v) => { __store[k] = v; }};
globalThis.__shown = []; globalThis.__permissionAsks = 0;
globalThis.Notification = class { constructor(title, o){ __shown.push({title, body: o.body}); } static permission = 'default';
  static async requestPermission(){ __permissionAsks++; Notification.permission = 'granted'; return 'granted'; } };
globalThis.window = {focus(){}};
`

// The Files list, a download, a session's title, its delete, the resume
// command and notifications: each draws untrusted text revealed, asks the
// server's routes, and shows nothing from the record outside the page.
func TestIDEFilesTitlesAndNotifications(t *testing.T) {
	if out, err := runConsoleCases(t, "ide-files", filesHarness, "ide_files_cases.mjs"); err != nil {
		t.Fatalf("the workbench's files, titles or notifications failed:\n%s", out)
	}
}

// uploadHarness adds to the chat harness an upload route that records what it
// was sent, and the browser's FormData and storage.
const uploadHarness = ideChatHarness + `
globalThis.FormData = class { constructor(){ this.f = {}; } append(k, v, name){ this.f[k] = v; if(name) this.f.name = name; } };
globalThis.__uploads = []; globalThis.__uploadError = ''; let __n = 0;
globalThis.fetch = async (url, opts) => {
  const f = opts.body.f; __uploads.push({url, dir: 'dir' in f ? f.dir : null, name: f.name});
  if(__uploadError) return {ok:false, status:413, statusText:'', json: async () => ({error: __uploadError})};
  const dot = f.name.lastIndexOf('.'), base = f.name.slice(0, dot).replace(/[^a-z]/g, ''), ext = f.name.slice(dot);
  return {ok:true, status:200, json: async () => ({path:'/w/uploads/s1/' + base + '-' + (++__n) + ext, name:f.name, kind: ext === '.png' ? 'image' : ''})};
};
const connLost = () => {}, signInEnded = () => {};
globalThis.__store = {}; const store = {get: (k, d) => k in __store ? __store[k] : d, set: (k, v) => { __store[k] = v; }};
globalThis.__shown = [];
`

// Files attached in the composer go to the session's upload route when the
// message is sent and are named in its prompt; an oversize or refused file
// sends nothing and stays attached. An approval notifies only on opt-in.
func TestIDEAttachmentsGoWithTheMessage(t *testing.T) {
	if out, err := runConsoleCases(t, "ide-chat", uploadHarness, "ide_upload_cases.mjs"); err != nil {
		t.Fatalf("the workbench's attachments failed:\n%s", out)
	}
}

// dropHarness adds to the tree harness an upload route that records what it was sent.
const dropHarness = ideTreeHarness + `
const visible = s => String(s), hawkSoon = () => {}, connLost = () => {}, signInEnded = () => {};
globalThis.FormData = class { constructor(){ this.f = {}; } append(k, v, name){ this.f[k] = v; if(name) this.f.name = name; } };
globalThis.__uploads = []; globalThis.__uploadError = '';
globalThis.fetch = async (url, opts) => {
  const f = opts.body.f; __uploads.push({url, dir: 'dir' in f ? f.dir : null, name: f.name});
  if(__uploadError) return {ok:false, status:403, statusText:'', json: async () => ({error: __uploadError})};
  return {ok:true, status:200, json: async () => ({path: (f.dir ? f.dir + '/' : '') + f.name})};
};
`

// A file dropped on an Explorer folder, or on a file in it, goes to the
// upload route with that folder; text drags, oversize files and refusals do not.
func TestIDEExplorerTakesDroppedFiles(t *testing.T) {
	if out, err := runConsoleCases(t, "ide-tree", dropHarness, "ide_drop_cases.mjs"); err != nil {
		t.Fatalf("the explorer's drop failed:\n%s", out)
	}
}
