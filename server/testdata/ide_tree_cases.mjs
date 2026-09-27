// Assertions driven against the workbench's explorer tree and name input,
// spliced in above this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const tick = (ms = 5) => new Promise(r => setTimeout(r, ms));
const inputOf = () => __root.querySelector('ren');

// New file with a collapsed folder selected: the folder opens, and its listing
// arrives after the name input is already in it.
await loadTree('', __root, 0);
treeSel = {path:'src', dir:true, name:'src'};
await newEntry(false);
const input = inputOf();
check('the name input is shown', !!input && input.isConnected);
await tick(60);
check('the folder\'s listing arriving later keeps the input', !!inputOf() && inputOf() === input && input.isConnected);
check('nothing raised a page error', __errors.length === 0);
input.value = 'new.txt'; input.on.keydown({key:'Enter', preventDefault(){}, stopPropagation(){}});
await tick(60);
check('Enter creates the file in the folder', __puts.includes('src/new.txt'));
check('and the held listing is drawn after it', !inputOf() && __root.querySelectorAll('row').some(r => r.dataset.path === 'src/main.go'));
check('still no page error', __errors.length === 0);

if(!ok) process.exit(1);
