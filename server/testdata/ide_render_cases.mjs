// Assertions driven against the workbench's real chat render(), spliced in
// above this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const ev = (seq, type, actor, payload) => ({seq, type, actor, payload, created_at: new Date().toISOString()});
const text = () => __added.map(n => n.textContent).join('\n');

// The person's shell, a line refused at their terminal, and a save.
render(ev(1, 'action.requested', 'user', {call_id:'u1', tool:'bash', args:{command:'bash -i', interactive:true}}));
render(ev(2, 'action.approved', 'system', {call_id:'u1', step:'mode', by:'user'}));
render(ev(3, 'action.requested', 'user', {call_id:'u2', tool:'bash', args:{command:'shutdown now'}}));
render(ev(4, 'action.denied', 'system', {call_id:'u2', step:'deny', reason:'denied'}));
render(ev(5, 'observation', 'tool', {call_id:'u1', tool:'bash', content:'exit 0 · interactive terminal', duration_ms:1086487}));
render(ev(6, 'action.requested', 'user', {call_id:'u3', tool:'write', args:{path:'/w/a.txt', content:'x'}}));
render(ev(7, 'observation', 'tool', {call_id:'u3', tool:'write', content:'wrote'}));
check('the person\'s own calls are not in the chat', __added.length === 0);
check('they are still in the event log', __logged === 7);
check('a save by hand still refreshes the changes view', __changes > 0);

// The agent's call is drawn as before.
render(ev(8, 'action.requested', 'agent', {call_id:'a1', tool:'bash', args:{command:'ls'}}));
render(ev(9, 'observation', 'tool', {call_id:'a1', tool:'bash', content:'a.txt', duration_ms:12}));
check('the agent\'s call is in the chat', __added.length === 1 && text().includes('ls') && text().includes('12ms'));
check('only the agent\'s command goes to its terminal', __agentTerm.length === 1 && __agentTerm[0] === 'ls');

if(!ok) process.exit(1);
