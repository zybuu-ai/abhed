// Assertions driven against the workbench's real lspRel(), spliced in above
// this file's contents by lsp_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const c = {root: '/w/repo'};
check('a workspace file is its relative path', lspRel(c, 'file:///w/repo/src/a.go') === 'src/a.go');
check('.. inside the workspace is resolved', lspRel(c, 'file:///w/repo/src/../b.go') === 'b.go');
check('.. that climbs out is not offered', lspRel(c, 'file:///w/repo/../etc/passwd') === null);
check('an encoded .. that climbs out is not offered', lspRel(c, 'file:///w/repo/%2e%2e/%2e%2e/etc/passwd') === null);
check('a sibling folder with the same prefix is not offered', lspRel(c, 'file:///w/repo2/a.go') === null);
check('the root itself is not a file', lspRel(c, 'file:///w/repo/.') === null);
if(!ok) process.exit(1);
