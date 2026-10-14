// The Events and HawkEYE panels draw record text, which is the model's and
// the tools': bidi and zero-width characters are shown, never obeyed.
let ok = true;
function check(label, pass){ console.log((pass ? 'PASS' : 'FAIL') + '  ' + label); ok = ok && pass; }
const raw = /[\u200b-\u200f\u202a-\u202e\u2066-\u2069\u3164\u2800]/;
logEvent({seq:4, type:'action.requested'}, {tool:'bash', args:{command:';fs- mr\u202e x\u200b'}});
logEvent({seq:5, type:'agent.message'}, {text:'done\u2066 \u3164'});
const ev = $('p-events').textContent;
check('the Events panel shows bidi and zero-width characters: ' + ev, !raw.test(ev) && ev.includes('⟨U+202E⟩') && ev.includes('⟨U+3164⟩'));
await loadHawkeye();
const hk = $('hawkeye').textContent + $('p-problems').textContent;
check('the HawkEYE panel shows them too: ' + hk, !raw.test(hk) && hk.includes('⟨U+202E⟩') && hk.includes('⟨U+200B⟩'));
// An MCP server that runs unconfined on the none tier says so in the
// Extensions panel, with the reason; a confined one carries no such mark.
caps = {permissions:{mode:'default', deny:[], ask:[], allow:[]}, tools:[], skills:[], extensions:[],
  mcp:[{name:'probe (1 tools)', status:'connected', unconfined:'the sandbox tier is none: chosen in the configuration'}, {name:'safe (2 tools)', status:'connected'}]};
drawCaps();
const xs = $('extensions').textContent;
check('the Extensions panel marks the unconfined server: ' + xs, xs.includes('unconfined') && xs.includes('the sandbox tier is none: chosen in the configuration') && xs.includes('home folder'));
check('and only that one', xs.split('unconfined').length === 2);
if(!ok) process.exit(1);
