// The Events and HawkEYE panels draw record text, which is the model's and
// the tools': bidi and zero-width characters are shown, never obeyed.
let ok = true;
function check(label, pass){ console.log((pass ? 'PASS' : 'FAIL') + '  ' + label); ok = ok && pass; }
const raw = /[​-‏‪-‮⁦-⁩ㅤ⠀]/;
logEvent({seq:4, type:'action.requested'}, {tool:'bash', args:{command:';fs- mr‮ x​'}});
logEvent({seq:5, type:'agent.message'}, {text:'done⁦ ㅤ'});
const ev = $('p-events').textContent;
check('the Events panel shows bidi and zero-width characters: ' + ev, !raw.test(ev) && ev.includes('⟨U+202E⟩') && ev.includes('⟨U+3164⟩'));
await loadHawkeye();
const hk = $('hawkeye').textContent + $('p-problems').textContent;
check('the HawkEYE panel shows them too: ' + hk, !raw.test(hk) && hk.includes('⟨U+202E⟩') && hk.includes('⟨U+200B⟩'));
if(!ok) process.exit(1);
