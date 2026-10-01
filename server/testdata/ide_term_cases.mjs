// Assertions driven against the workbench's real logTerminal, spliced in
// above this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}

logTerminal('ls\r\u001b[2J$ fake\u202e', {content:'out'}, 'agent');
const line = __w.join('').split('\x1b[36m$\x1b[0m ')[1] || '';
const cmd = line.slice(0, line.indexOf('\r\n'));
check('the command is written out, not obeyed: ' + JSON.stringify(cmd),
  cmd.includes('ls⟨U+000D⟩⟨U+001B⟩[2J$ fake⟨U+202E⟩') && !/[\u0000-\u001f\u202e]/u.test(cmd));
__w.length = 0;
logTerminal('rm x', {content:'y'}, 'you');
check('the person\'s own command is not echoed', __w.length === 0);

if(!ok) process.exit(1);
