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

if(!ok) process.exit(1);
