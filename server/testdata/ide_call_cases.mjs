// Assertions driven against the workbench's real fillCall, clip and drawPlan,
// spliced in above this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const HID = ['\u202e', '\u2066', '\u2069', '\u200d', '\u200b', '\u0007'];
const raw = s => HID.some(c => s.includes(c));

// A call's output is untrusted: the description echoed in "Started in background"
// is drawn with its bidi, isolate, joiner, zero-width and BEL characters written out,
// and the output keeps its newlines, tabs and indentation.
const out = 'Started in background: t7 · scan\u202egol\u2066x\u2069 a\u200db\u200bc\u0007\n    indented\tline';
const node = new El('details'); node.appendChild(new El('summary'));
const c = {node, args:{description:'scan\u202egol'}, out, drawn:0};
fillCall(c);
const shown = node.textContent;
check('an opened call shows hidden characters in its output and arguments: ' + JSON.stringify(shown),
  !raw(shown) && shown.includes('scan⟨U+202E⟩gol⟨U+2066⟩x⟨U+2069⟩ a⟨U+200D⟩b⟨U+200B⟩c⟨U+0007⟩\n    indented\tline'));

// Long output shows its head, and the rest on asking, written out the same way.
const long = Array.from({length: 30}, (_, i) => 'l' + i + '\u202e').join('\n');
const wrap = clip(long), more = wrap.childNodes[1];
check('long output shows its head with hidden characters written out', !raw(wrap.textContent) && wrap.textContent.includes('l0⟨U+202E⟩'));
more.on.click();
check('the rest of long output is written out too', !raw(wrap.childNodes[0].textContent) && wrap.childNodes[0].textContent.includes('l29⟨U+202E⟩'));

// The plan is the model's text.
drawPlan([{status:'done', text:'step\u202eone'}]);
check('a plan item shows hidden characters', !raw(__added[0].textContent) && __added[0].textContent.includes('step⟨U+202E⟩one'));

if(!ok) process.exit(1);
