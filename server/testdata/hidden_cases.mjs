// Assertions driven against the page's real hasHidden(), spliced in above this
// file's contents by console_render_test.go; CASES comes from Go's ArgsHidden.
let ok = true;
for(const c of CASES){
  const got = hasHidden(JSON.parse(c.raw));
  console.log((got === c.want ? 'PASS' : 'FAIL') + '  nested ' + c.depth + ': ' + got + ', Go says ' + c.want);
  ok = ok && got === c.want;
}
if(!ok) process.exit(1);
