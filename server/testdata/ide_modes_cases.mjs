// The mode selector offers only what the server lets a session start in.
let ok = true;
function check(label, pass){ console.log((pass ? 'PASS' : 'FAIL') + '  ' + label); ok = ok && pass; }
const on = () => sel.options.filter(o => !o.disabled).map(o => o.value).join(',');
limitModes('accept-edits');
check('an accept-edits server offers accept-edits and plan: ' + on(), on() === 'plan,accept-edits' && sel.value === 'accept-edits');
limitModes('default');
check('a managed default offers default and plan: ' + on(), on() === 'default,plan' && sel.value === 'default');
limitModes('bypass');
check('a mode the list lacks is added and chosen: ' + on(), on() === 'plan,bypass' && sel.value === 'bypass');
if(!ok) process.exit(1);
