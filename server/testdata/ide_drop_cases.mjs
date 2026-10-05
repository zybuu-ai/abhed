// Assertions driven against files dropped on the explorer's tree, spliced in
// above this file by ide_files_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const tick = (ms = 5) => new Promise(r => setTimeout(r, ms));
const drag = (files, types) => ({dataTransfer:{types: types || ['Files'], files, dropEffect:''}, prevented:false, preventDefault(){ this.prevented = true; }, stopPropagation(){}});
const rowOf = path => __root.querySelectorAll('row').find(r => r.dataset.path === path);

await loadTree('', __root, 0);
const src = rowOf('src');
let ev = drag([]); src.on.dragover(ev);
check('a folder row takes a drag of files', ev.prevented && src.className.includes('drop'));
src.on.dragleave(); ev = drag([{name:'a.png', size:4}]); src.on.drop(ev); await tick(80);
check('a file dropped on a folder is put into it, under its name', __uploads.length === 1 && __uploads[0].url === '/v1/sessions/s1/upload' && __uploads[0].dir === 'src' && __uploads[0].name === 'a.png');
check('and the folder is opened to show it', expanded.has('src'));

const main = rowOf('src/main.go');
main.on.drop(drag([{name:'b.txt', size:1}])); await tick(80);
check('a file dropped on a file goes to that file\'s folder', __uploads.length === 2 && __uploads[1].dir === 'src');

ev = drag([], ['text/plain']); src.on.dragover(ev); src.on.drop(ev); await tick();
check('a drag of text is left alone', !ev.prevented && __uploads.length === 2);

src.on.drop(drag([{name:'big.iso', size:33 * 1024 * 1024}])); await tick(80);
check('an oversize file is refused before upload', __uploads.length === 2 && __errors.some(m => m.includes('big.iso is larger than the 32 MB limit')));

__uploadError = 'Denied: write(**/locked/**)';
src.on.drop(drag([{name:'c.txt', size:1}])); await tick(80);
check('a refusal by the server is shown', __errors.some(m => m === 'c.txt: Denied: write(**/locked/**)'));

if(!ok) process.exit(1);
