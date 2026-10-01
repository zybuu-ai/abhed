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

// A fork marker draws a divider naming the step the conversation went on from.
__added.length = 0;
render(ev(20, 'conversation.forked', 'user', {through_seq:7}));
check('a fork marker is drawn', text().includes('forked at step 7'));

// A background result and the closing end.
__added.length = 0;
render(ev(30, 'subagent.notice', 'system', {task_id:'t1', description:'scan logs', status:'completed', turns:1, content:'three errors'}));
render(ev(31, 'session.ended', 'system', {reason:'completed', background:0, settled:true}));
check('a background result is drawn with its summary', text().includes('background: scan logs finished (completed, 1 turn)') && text().includes('three errors'));
check('the closing end is drawn as background work finishing', text().includes('background work finished'));
// Background text is the model's or a command's: bidi and zero-width characters are written out, as on an ask card.
const RLO = String.fromCharCode(0x202e), ZW = String.fromCharCode(0x200b);
__added.length = 0;
render(ev(40, 'subagent.spawned', 'system', {task_id:'t2', description:'scan' + RLO + 'gol', background:true}));
const bar = $('s-bg');
const barOk = bar.textContent.includes('⟨U+202E⟩') && !bar.textContent.includes(RLO) && bar.title.includes('⟨U+202E⟩') && !bar.title.includes(RLO);
render(ev(41, 'subagent.returned', 'system', {reason:'done' + ZW}));
render(ev(42, 'subagent.notice', 'system', {task_id:'t2', description:'scan' + RLO + 'gol', status:'completed' + ZW, content:'line one\nok' + RLO + 'txt.exe'}));
render(ev(43, 'session.woken', 'system', {by:'policy', task_ids:['t2']}));
const shown = text() + bar.title;
check('background names, results and reasons show bidi and zero-width as code points',
  !shown.includes(RLO) && !shown.includes(ZW) && text().includes('subagent started: scan⟨U+202E⟩gol') &&
  text().includes('finished (completed⟨U+200B⟩') && text().includes('line one\nok⟨U+202E⟩txt.exe') &&
  text().includes('continuing with results from scan⟨U+202E⟩gol') && barOk);
// A background shell is listed in the status bar while it runs, and leaves it when it ends.
bgTasks.clear();
render(ev(50, 'shell.started', 'system', {shell_id:'sh_1', call_id:'c1', command:'npm run dev', description:'dev server'}));
const runningShell = $('s-bg').textContent;
render(ev(51, 'shell.ended', 'system', {shell_id:'sh_1', call_id:'c1', state:'exited', exit_code:0}));
check('a background shell is in the status bar while it runs', $('s-bg').hidden === true && runningShell.includes('1 background: dev server') &&
  $('s-bg').textContent === '' && $('s-bg').title.includes('dev server · exited'));
// A decision's reason and scope sit under the call they settle, and are drawn written out too.
__added.length = 0;
const R = String.fromCharCode(0x202e), J = String.fromCharCode(0x200d);
render(ev(60, 'action.requested', 'agent', {call_id:'a6', tool:'bash', args:{command:'ls' + J}}));
render(ev(61, 'action.approved', 'system', {call_id:'a6', step:'rule', granted_scope:'bash:ls' + R, reason:'ok' + R}));
render(ev(62, 'action.requested', 'agent', {call_id:'a7', tool:'bash', args:{command:'rm'}}));
render(ev(63, 'action.denied', 'system', {call_id:'a7', step:'deny', reason:'no' + R + 'pe'}));
const why = __added.map(n => n.textContent).join('\n');
check('a decision\'s reason and scope show hidden characters: ' + JSON.stringify(why),
  !why.includes(R) && !why.includes(J) && why.includes('ls⟨U+200D⟩') && why.includes('always allowing bash:ls⟨U+202E⟩ — ok⟨U+202E⟩') && why.includes('— no⟨U+202E⟩pe'));
// A model error quotes the model's own invalid arguments: an RLO in them must not reverse the row.
__added.length = 0;
render(ev(70, 'model.call', 'system', {turn:1, model:'m', error:'model produced invalid JSON arguments for bash: {"description": "bd2' + R + 'gpj.exe' + String.fromCharCode(0x200b) + '"}'}));
const merr = text();
check('a model error shows hidden characters: ' + JSON.stringify(merr),
  !merr.includes(R) && merr.includes('model error: model produced invalid JSON arguments for bash: {"description": "bd2⟨U+202E⟩gpj.exe⟨U+200B⟩"}'));
if(!ok) process.exit(1);
