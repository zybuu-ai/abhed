// Assertions driven against the workbench's real line-by-line terminal,
// spliced in above this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const tick = () => new Promise(r => setTimeout(r, 5));
const term = () => {
  const out = [];
  return {out, t:{term:{write:s => out.push(s), cols:80, rows:24}, line:'', queue:[], hist:[], at:0, cwd:'.', mode:'lines'}};
};
const CONFIRM = {confirm:'recursive/forced delete — always requires confirmation', cwd:'.'};

// y runs the line, sent again with confirmed; the prompt says why.
let {out, t} = term();
__sent.length = 0; __replies.push(CONFIRM, {id:'u2', cwd:'.'});
linesData(t, 'rm -rf x\r'); await tick();
check('a destructive line is sent once, with no answer', __sent.length === 1 && !('confirmed' in __sent[0]) && !('declined' in __sent[0]));
check('the prompt shows the reason and asks', t.confirm === 'rm -rf x' && out.join('').includes('always requires confirmation') && out.join('').includes('Run it? [y/N]'));
check('arrow keys do nothing at the prompt', lineKeys(t, {type:'keydown', key:'ArrowUp'}) === false && t.line === '');
linesData(t, 'y'); linesData(t, '\r'); await tick();
check('y sends it again, confirmed', __sent.length === 2 && __sent[1].confirmed === true && !('declined' in __sent[1]) && __sent[1].command === 'rm -rf x');
check('a confirmed line runs', __attached === 'u2' && !t.confirm);
check('the line is in the history once', t.hist.length === 1);

// Anything but y declines, and clears the lines queued behind it.
for(const answer of ['yes\r', '\r', 'n\r', '\x03']){
  ({out, t} = term());
  __sent.length = 0; __attached = null; __replies.push(CONFIRM, {id:'u3', denied:'Not run: not confirmed', cwd:'.'});
  linesData(t, 'rm -rf x\recho next\r'); await tick();
  check(JSON.stringify(answer) + ': one line waits behind the prompt', t.queue.length === 1);
  linesData(t, answer); await tick();
  check(JSON.stringify(answer) + ' declines', __sent.length === 2 && __sent[1].declined === true && !('confirmed' in __sent[1]));
  check(JSON.stringify(answer) + ' runs nothing and drops the queue', __attached === null && t.queue.length === 0 && __sent.every(b => b.command !== 'echo next'));
}

// Type-ahead that was not entered survives the prompt.
({out, t} = term());
__sent.length = 0; __replies.push(CONFIRM, {id:'u4', cwd:'.'});
linesData(t, 'rm -rf x\r'); t.line = 'git st'; await tick();
check('type-ahead is set aside at the prompt', t.confirm && t.line === '');
linesData(t, 'y\r'); await tick();
check('type-ahead comes back after the answer', t.line === 'git st');

// An ordinary line runs on Enter, as before.
({out, t} = term());
__sent.length = 0; __attached = null; __replies.push({id:'u5', cwd:'.'});
linesData(t, 'ls\r'); await tick();
check('an ordinary line runs on Enter', __sent.length === 1 && __attached === 'u5' && !t.confirm);

process.exit(ok ? 0 : 1);
