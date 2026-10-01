// Assertions driven against the console's real loadMode(), spliced in above
// this file's contents by console_render_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const offered = () => sel.options.filter(o => !o.disabled).map(o => o.value).sort().join(',');

__caps = {permissions:{mode:'accept-edits'}};
await loadMode();
check('the selector starts on the configured mode', sel.value === 'accept-edits');
check('and offers only it and plan', offered() === 'accept-edits,plan');

reset(); __caps = {permissions:{mode:'bypass'}};
await loadMode();
check('a configured mode the list lacks is added and chosen', sel.value === 'bypass' && offered() === 'bypass,plan');

reset(); __caps = null;
await loadMode();
check('with no answer the selector is left as it was', sel.value === 'default' && offered() === 'accept-edits,auto,default,plan');

if(!ok) process.exit(1);
