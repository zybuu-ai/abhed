// Assertions driven against the console's model picker, spliced in above this
// file's contents by console_render_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const sel = $('mdlpick');
const lastNote = () => { const n = $('tx').childNodes; return n.length ? n[n.length - 1].textContent : ''; };

await loadProviders();
check('the picker is shown with the default chosen', !sel.hidden && sel.value === 'a');
check('the default is not sent with a new chat', chosenProvider() === undefined);

sel.value = 'b'; await sel.onchange();
check('with no chat open, the choice is kept for the next one', chosenProvider() === 'b' && __posted.length === 0);

current = 's1'; sessionsSeen = [{id:'s1', model:'model-a'}];
showSessionModel('s1');
check('opening a chat shows the model it runs on', sel.value === 'a');

sel.value = 'b'; await sel.onchange();
check('a switch is sent for the open chat', __posted.length === 1 && __posted[0].url === '/v1/sessions/s1/model' && __posted[0].body.provider === 'b');
check('the switch names both models', lastNote() === 'model switched to model-b (was model-a)');

__fail = 'the session is mid-turn; interrupt it or wait for the turn to finish';
sel.value = 'a'; await sel.onchange();
check('a refused switch says why', lastNote().startsWith('Model not switched: the session is mid-turn'));
check('a refused switch shows the model still in use', sel.value === 'b');

// Two providers serving one model are told apart by name, in the picker and the note.
__fail = null; __posted.length = 0;
globalThis.__providers = [{name:'a', model:'m', default:true}, {name:'b', model:'m', default:false}, {name:'c', model:'other', default:false}];
await loadProviders();
const labels = sel.childNodes.filter(o => o.value).map(o => o.textContent);
check('options for a shared model name the provider', labels.join('|') === 'a · m|b · m|other');
recProvider = 'a'; globalThis.__reply = {provider:'b', model:'m', from:'m'};
sel.value = 'b'; await sel.onchange();
check('the switch note names both providers', lastNote() === 'model switched to b · m (was a · m)');
// The recorded event can be drawn before the reply returns; the reply then adds nothing.
sel.value = 'a'; globalThis.__reply = {provider:'a', model:'m', from:'m'}; recProvider = 'b';
note(switchedText(__reply, 'b'));
const notes = $('tx').childNodes.length;
await sel.onchange();
check('a switch its event already drew is not a second line', $('tx').childNodes.length === notes);

if(!ok) process.exit(1);
