// Assertions on how the console names a session in its list, spliced in
// above this file's contents by console_render_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
check('a done session is named by how it ended', shownState({id:'s2', state:'done', reason:'shutdown'}) === 'shutdown');
check('a done session with no recorded reason stays done', shownState({id:'s2', state:'done'}) === 'done');
check('a live session keeps its state', shownState({id:'s2', state:'running'}) === 'running');
current = 's1'; live = false; stats.reason = 'shutdown';
check('the open session ended by a drain shows shutdown before the list catches up', shownState({id:'s1', state:'running'}) === 'shutdown');
check('another session keeps its listed state', shownState({id:'s2', state:'running'}) === 'running');
live = true;
check('a live open session keeps its listed state', shownState({id:'s1', state:'running'}) === 'running');

// A drain ends the open session while the list can no longer be fetched: the
// rail's pill changes when session.ended renders.
const row = (id, state) => { const r = new El('div'); r.className = 'item'; r.dataset.id = id;
  const p = new El('span'); p.className = 'pill ' + state; p.dataset.state = state; p.textContent = state; r.appendChild(p); $('list').appendChild(r); return p; };
const open = row('s1', 'running'), other = row('s2', 'running');
current = 's1'; live = false; stats.reason = 'shutdown';
paintOpenPill();
check('the open session\'s pill shows the drain as it ends', open.className === 'pill shutdown' && open.textContent === 'shutdown');
check('another session\'s pill is left alone', other.className === 'pill running' && other.textContent === 'running');
const badges = listBadges({background:2, pending_ask:{tool:'bash', subagent:'scan'}}).map(b => b.textContent);
check('a row names its background tasks and a waiting approval', badges.join('|') === 'background 2|approval waiting');
check('a quiet row has no badges', listBadges({state:'done'}).length === 0);
if(!ok) process.exit(1);
