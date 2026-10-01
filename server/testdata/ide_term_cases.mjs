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
// So is its output: a description echoed back, or an escape that would clear the screen.
__w.length = 0;
logTerminal('task', {content:'Started in background: t7 · scan' + String.fromCharCode(0x202e) + 'gol' + String.fromCharCode(0x200d) + '\x1b[2J\x07\n\tok'}, 'agent');
const printed = __w.join('').split('\x1b[36m$\x1b[0m task\r\n')[1] || '';
check('the output is written out, not obeyed: ' + JSON.stringify(printed),
  printed.startsWith('Started in background: t7 · scan⟨U+202E⟩gol⟨U+200D⟩⟨U+001B⟩[2J⟨U+0007⟩\n\tok') && !/[\u0000-\u0008\u000b-\u001f\u200d\u202e]/u.test(printed.replace(/\r\n$/, '')));
__w.length = 0;
logTerminal('rm x', {content:'y'}, 'you');
check('the person\'s own command is not echoed', __w.length === 0);

if(!ok) process.exit(1);
