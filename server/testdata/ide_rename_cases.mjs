// Assertions driven against the workbench's real renameEntry(), spliced in
// above this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const tick = (ms = 5) => new Promise(r => setTimeout(r, ms));
const inputOf = () => __root.querySelector('ren');
const enter = i => i.on.keydown({key:'Enter', preventDefault(){}, stopPropagation(){}});
const RLO = String.fromCharCode(0x202e);
await loadTree('', __root, 0);

// A plain name is offered as it is, to edit.
const plainRow = __root.querySelectorAll('row').find(r => r.dataset.path === 'src');
renameEntry({name:'src', path:'src', dir:true}, plainRow);
check('a plain name is put in the box to edit', inputOf().value === 'src' && !inputOf().placeholder);
inputOf().on.keydown({key:'Escape', preventDefault(){}, stopPropagation(){}}); await tick();

// A name with hidden characters is not put in the box raw.
listings[''] = [{name:'notes' + RLO + 'dm.txt', path:'notes' + RLO + 'dm.txt'}];
await loadTree('', __root, 0);
const evilRow = __root.querySelectorAll('row')[0];
renameEntry({name:'notes' + RLO + 'dm.txt', path:'notes' + RLO + 'dm.txt'}, evilRow);
const box = inputOf();
check('a name with hidden characters is not seeded into the box', box.value === '');
check('the hint shows the name with them written out', box.placeholder === 'new name for notes⟨U+202E⟩dm.txt' && !box.placeholder.includes(RLO));
box.value = 'notes.txt'; enter(box); await tick(40);
check('a name typed in its place renames it', __renames.length === 1 && __renames[0].from === 'notes' + RLO + 'dm.txt' && __renames[0].to === 'notes.txt');
// Leaving the empty box renames nothing.
renameEntry({name:'notes' + RLO + 'dm.txt', path:'notes' + RLO + 'dm.txt'}, __root.querySelectorAll('row')[0]);
enter(inputOf()); await tick(40);
check('an empty box renames nothing', __renames.length === 1);
if(!ok) process.exit(1);
