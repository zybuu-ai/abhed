// Assertions driven against the workbench's real resetSession(), spliced in
// above this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const q = $('q'); q.placeholder = 'Describe a change, or type / for commands';
offerNext('Run the tests again');
check('a session\'s suggestion is offered in the box', q.placeholder === 'Run the tests again');
resetSession();
check('a new session does not show the last one\'s suggestion', q.placeholder === 'Describe a change, or type / for commands' && !q.dataset.next);
if(!ok) process.exit(1);
