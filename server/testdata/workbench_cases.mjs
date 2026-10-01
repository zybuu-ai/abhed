// Assertions driven against the workbench's real drawing functions, spliced in
// above this file's contents by console_render_test.go.
const HOSTILE = '<img src=x onerror=alert(1)><script>alert(2)</script>';

// Anything written through innerHTML leaves its markup behind in the shim.
function markupSet(n){
  if(n.nodeType !== 1) return false;
  return Boolean(n._html) || n.childNodes.some(markupSet);
}

let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}

showFile({path: HOSTILE, size: 120, binary: false, truncated: false,
  content: HOSTILE + '\nsecond line\n'});
check('a file is drawn as text, markup and all',
  main.textContent.includes('1' + HOSTILE + '\n2second line\n') && !markupSet(main));
check('a file path is drawn as text', main.querySelector('.nm').textContent === HOSTILE);
check('one gutter number per line', main.querySelectorAll('.ln').length === 2);

showFile({path: 'logo.png', size: 2048, binary: true, truncated: false, content: ''});
check('a binary file shows a note and no content',
  main.textContent.includes('Binary file') && main.querySelectorAll('.ln').length === 0);

showFile({path: 'big.log', size: 3 * 1048576, binary: false, truncated: true, content: 'x\n'});
check('a truncated file says so', main.textContent.includes('3.0 MB') && main.textContent.includes('Only the first'));

viewDiff({path: 'a.sh', status: 'modified', diff:
  '--- a/a.sh\n+++ b/a.sh\n@@ -1,4 +1,4 @@\n keep\n---flag\n+++flag\n-' + HOSTILE + '\n+safe\n'});
const classes = main.querySelector('.diff').childNodes.map(n => n.className);
check('diff lines are classed by what they are, headers by position',
  JSON.stringify(classes) === JSON.stringify(['meta', 'meta', 'hunk', '', 'del', 'add', 'del', 'add']));
check('a diff is drawn as text, markup and all',
  main.textContent.includes('-' + HOSTILE) && !markupSet(main));

// A file's name, its lines and a diff are the agent's to write: hidden characters are written out.
const RLO = '\u202e', ZWJ = '\u200d';
showFile({path: 'gpj' + RLO + '.exe', size: 9, binary: false, truncated: false, content: 'a' + RLO + 'b\n\tc' + ZWJ + '\n'});
const fileText = main.textContent + main.querySelector('.nm').title;
check('a file\'s name and lines show hidden characters, and keep their tabs',
  !fileText.includes(RLO) && !fileText.includes(ZWJ) && main.querySelector('.nm').textContent === 'gpj⟨U+202E⟩.exe' &&
  main.textContent.includes('1a⟨U+202E⟩b\n2\tc⟨U+200D⟩\n'));
viewDiff({path: 'x' + RLO, status: 'modified', diff: '--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new' + RLO + '\n'});
check('a diff shows hidden characters', !main.textContent.includes(RLO) && main.textContent.includes('+new⟨U+202E⟩'));

viewDiff({path: 'logo.png', status: 'modified', diff: '', note: 'binary'});
check('a change with no diff says why', main.textContent.includes('No diff: binary.'));

process.exit(ok ? 0 : 1);
