// Assertions driven against the workbench's real drawAt() and drawPal(),
// spliced in above this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const RLO = '\u202e', ZW = '\u200b';
known.add('src/notes' + RLO + 'dm.txt'); known.add('src/main.go'); known.add('docs/a' + ZW + 'b.md');
if(typeof drawAt === 'function'){
  const q = $('q'); q.value = 'look at @src'; q.selectionStart = q.selectionEnd = q.value.length;
  drawAt();
  const rows = $('slash').childNodes.map(b => b.textContent);
  check('the @ list offers the matching files', rows.length === 2 && rows.includes('@src/main.go'));
  check('a name with hidden characters shows them as code points', rows.includes('@src/notes⟨U+202E⟩dm.txt') && !rows.some(r => r.includes(RLO)));
  const evil = $('slash').childNodes.find(b => b.textContent.includes('notes'));
  evil.on.mousedown({preventDefault(){}});
  check('choosing one inserts the real path', q.value === 'look at @src/notes' + RLO + 'dm.txt ');
}
if(typeof drawPal === 'function'){
  $('palq').value = '';
  drawPal();
  const ls = $('pall').childNodes.map(b => b.childNodes[1] ? b.childNodes[1].textContent : '');
  check('the palette lists files with hidden characters written out', ls.includes('src/notes⟨U+202E⟩dm.txt') && ls.includes('docs/a⟨U+200B⟩b.md'));
  check('and sessions too', ls.includes('fix⟨U+202E⟩it'));
  check('nothing in the palette carries a raw hidden character', !ls.some(l => l.includes(RLO) || l.includes(ZW)));
  $('palq').value = 'notes'; drawPal();
  const hit = $('pall').childNodes[0];
  hit.on.click();
  check('choosing a file opens the real path', __opened[0] === 'src/notes' + RLO + 'dm.txt');
}
if(!ok) process.exit(1);
