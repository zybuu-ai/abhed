// Assertions driven against the workbench's model picker, spliced in above
// this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const sel = $('mdlpick');
const lastLine = () => { const n = __root.childNodes; return n.length ? n[n.length - 1].textContent : ''; };

await loadProviders();
check('the picker is shown with the default chosen', !sel.hidden && sel.value === 'a');
check('the default is not sent with a new session', chosenProvider() === undefined);

sel.value = 'b'; await switchModel();
check('with no session open, the choice is kept for the next one', chosenProvider() === 'b' && __posted.length === 0);
check('the status bar names the model the next session starts on', $('s-model').textContent === 'model-b');

current = 's1'; sessionList = [{id:'s1', model:'model-a'}];
showSessionModel(sessionList[0]);
check('opening a session shows the model it runs on', sel.value === 'a' && $('s-model').textContent === 'model-a');

sel.value = 'b'; await switchModel();
check('a switch is sent for the open session', __posted.length === 1 && __posted[0].url === '/v1/sessions/s1/model' && __posted[0].body.provider === 'b');
check('the status bar and the list follow it', $('s-model').textContent === 'model-b' && sessionList[0].model === 'model-b');

__fail = 'the session is mid-turn; interrupt it or wait for the turn to finish';
sel.value = 'a'; await switchModel();
check('a refused switch says why', lastLine().startsWith('Model not switched: the session is mid-turn'));
check('a refused switch shows the model still in use', sel.value === 'b');
__fail = 'bad \u202e name';
sel.value = 'a'; await switchModel();
check('a refused switch draws its reason through the helper', lastLine() === 'Model not switched: bad \u27e8U+202E\u27e9 name');

// Two providers serving one model are told apart by name, in the picker and the note.
__fail = null;
globalThis.__providers = [{name:'a', model:'m', default:true}, {name:'b', model:'m', default:false}, {name:'c', model:'other', default:false}];
await loadProviders();
const labels = sel.childNodes.filter(o => o.value).map(o => o.textContent);
check('options for a shared model name the provider', labels.join('|') === 'a · m|b · m|other');
recProvider = null;
showSwitch({provider:'a', model:'m', from:'other'});
check('a switch from an unnamed provider names the model it left', lastLine() === 'model switched to a · m (was other)');
showSwitch({provider:'b', model:'m', from:'m'});
check('a switch names both providers', lastLine() === 'model switched to b · m (was a · m)');
check('the picker follows the switch', sel.value === 'b');
// Two switches between providers of one model read as two different lines.
const lines = __root.childNodes.length;
showSwitch({provider:'a', model:'m', from:'m'});
check('switching back is its own line, naming both providers', __root.childNodes.length === lines + 1 && lastLine() === 'model switched to a · m (was b · m)');

if(!ok) process.exit(1);
