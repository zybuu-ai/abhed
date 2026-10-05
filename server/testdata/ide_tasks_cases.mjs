// Assertions driven against the workbench's real showTasks(), spliced in above
// this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const rows = () => $('ctx').childNodes;

__tasks = [
  {task_id:'sh_1', kind:'shell', description:'watch the build', command:'make watch', status:'running', last_line:'built 3 files'},
  {task_id:'t_2', kind:'task', description:'scan‮logs', status:'completed'},
  {task_id:'sh_3', kind:'shell', description:'sleep 9', status:'killed', reason:'user_interrupt', exit_code:137},
];
await showTasks(null);
check('every task is listed, named by kind', rows().length === 3 && rows()[0].textContent.startsWith('shell · watch the build') && rows()[1].textContent.startsWith('task · '));
check('a running one offers Cancel', rows()[0].textContent.endsWith('Cancel') && !rows()[0].disabled && __focused === rows()[0]);
check('an ended one says how it ended and offers nothing', rows()[2].disabled === true && rows()[2].textContent.endsWith('killed 137 · user_interrupt'));
check('a name is drawn with its hidden characters revealed', rows()[1].textContent.includes('scan⟨U+202E⟩logs') && !rows()[1].textContent.includes('‮'));
check('the command and last line are in the tooltip', rows()[0].title === 'make watch\nbuilt 3 files');
await rows()[0].on.click();
check('Cancel cancels that task, and only that one', __routes.join(',') === 'GET /v1/sessions/s1/tasks,POST /v1/sessions/s1/tasks/sh_1/cancel');

__tasks = []; __routes.length = 0;
await showTasks(null);
check('no tasks says so', rows().length === 1 && rows()[0].disabled && rows()[0].textContent.startsWith('No background tasks'));

current = 's2'; __fail = 'your sign-in ended; sign in again';
await showTasks(null);
check('a list that cannot be fetched says why', __added.some(n => n.textContent === 'Background tasks not listed: your sign-in ended; sign in again'));

if(!ok) process.exit(1);
