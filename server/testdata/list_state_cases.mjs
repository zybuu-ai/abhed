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
if(!ok) process.exit(1);
