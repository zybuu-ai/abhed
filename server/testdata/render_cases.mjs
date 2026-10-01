// Assertions driven against the real render() spliced in above this file's
// contents by console_render_test.go.
const REPLY = 'RACF protects CICS transactions through resource profiles.';

function run(label, events){
  turnEl=null; streamEl=null; streamBody=null;
  tx.childNodes.length = 0;
  for(const ev of events) render(ev);
  const bubbles = tx.querySelectorAll('.said:not(.user)');
  const texts = bubbles.map(b => b.textContent.replace(/^abhed/,''));
  const dup = texts.filter(t => t.trim() === REPLY.trim()).length;
  console.log(`${dup===1?'PASS':'FAIL'}  ${label} -> ${bubbles.length} bubble(s), reply x${dup}`);
  if(dup!==1) texts.forEach((t,i)=>console.log(`        [${i}] ${JSON.stringify(t.slice(0,70))}`));
  return dup===1;
}

const deltas = () => REPLY.match(/.{1,12}/g).map((f,i)=>({seq:i+2,type:'agent.delta',payload:{text:f}}));
const user   = {seq:1,  type:'user.message',   payload:{text:'q'}};
const reason = {seq:90, type:'agent.reasoning',payload:{text:'thinking about it',turn:1}};
const msg    = {seq:99, type:'agent.message',  payload:{text:REPLY}};

let ok = true;
// The reported bug: reasoning is recorded once the turn's text is complete, so
// it lands between the deltas and agent.message. A handler that cleared the
// stream reference there made agent.message print the whole reply again.
ok = run('reasoning between deltas and message', [user,...deltas(),reason,msg]) && ok;
ok = run('no reasoning at all',                  [user,...deltas(),msg])        && ok;
ok = run('reasoning before any delta',           [user,reason,...deltas(),msg]) && ok;
ok = run('message with no deltas',               [user,msg])                    && ok;
ok = run('two reasoning events in one turn',     [user,reason,...deltas(),reason,msg]) && ok;
// A fork marker draws a divider naming the step the conversation went on from.
tx.childNodes.length = 0;
render({seq:120, type:'conversation.forked', payload:{through_seq:7}});
const forked = tx.textContent.includes('forked at step 7');
console.log((forked ? 'PASS' : 'FAIL') + '  a fork marker is drawn');
ok = forked && ok;
// A background result is drawn as the subagent's result, not as a message
// from the person; the closing end is not a second end of the run.
tx.childNodes.length = 0;
render({seq:129, type:'subagent.spawned', payload:{task_id:'t1', description:'scan logs', background:true}});
live = false;
render({seq:130, type:'subagent.notice', payload:{task_id:'t1', description:'scan logs', status:'completed', turns:3,
  delivery:'idle', content:'three errors in auth.log'}});
render({seq:131, type:'session.woken', payload:{by:'policy', task_ids:['t1']}});
const wokeLive = live;
render({seq:132, type:'session.ended', payload:{reason:'completed', background:0, settled:true}});
const bgText = tx.textContent;
const bgOk = bgText.includes('scan logs finished (completed, 3 turns)') && bgText.includes('three errors in auth.log') &&
  bgText.includes('the agent sees it with your next message') && bgText.includes('continuing with results from scan logs') && wokeLive === true &&
  bgText.includes('background work finished') && tx.querySelectorAll('.said.user').length === 0;
console.log((bgOk ? 'PASS' : 'FAIL') + '  a background result, a wake and the closing end are drawn as such');
ok = bgOk && ok;
// Background text is the model's or a command's: bidi and zero-width characters are written out.
const RLO = String.fromCharCode(0x202e), ZW = String.fromCharCode(0x200b);
tx.childNodes.length = 0; turnEl = null;
render({seq:140, type:'subagent.spawned', payload:{task_id:'t9', description:'scan' + RLO + 'gol', background:true}});
render({seq:141, type:'subagent.returned', payload:{reason:'done' + ZW}});
render({seq:142, type:'subagent.notice', payload:{task_id:'t9', description:'scan' + RLO + 'gol', status:'completed', content:'ok' + RLO + 'txt.exe'}});
render({seq:143, type:'session.woken', payload:{by:'policy', task_ids:['t9']}});
const hidden = tx.textContent;
const hiddenOk = !hidden.includes(RLO) && !hidden.includes(ZW) && hidden.includes('subagent started: scan⟨U+202E⟩gol') &&
  hidden.includes('ok⟨U+202E⟩txt.exe') && hidden.includes('done⟨U+200B⟩') && hidden.includes('continuing with results from scan⟨U+202E⟩gol');
console.log((hiddenOk ? 'PASS' : 'FAIL') + '  background names, results and reasons show bidi and zero-width as code points');
ok = hiddenOk && ok;
// A tool call's output, its peek and a reply are untrusted text: bidi, isolates, joiners,
// zero-width and BEL are written out wherever they are drawn, and newlines, tabs and
// indentation are kept. The description echoed in "Started in background" is one such output.
const HID = ['\u202e', '\u2066', '\u2069', '\u200d', '\u200b', '\u0007', '\u001b'];
const SPOOF = 'Started in background: t7 · scan\u202egol\u2066x\u2069 a\u200db\u200bc\u0007\u001b[2J';
const raw = s => HID.some(c => s.includes(c));
tx.childNodes.length = 0; turnEl = null; calls.clear();
render({seq:150, type:'action.requested', payload:{call_id:'c7', tool:'task', args:{description:'scan\u202egol'}}});
render({seq:151, type:'observation', payload:{call_id:'c7', tool:'task', content:SPOOF + '\n    indented\tline\n'}});
const call = calls.get('c7');
const peek = call.querySelector('.peek').textContent, out = call.querySelector('.out').textContent;
const outOk = !raw(peek) && !raw(out) && peek.includes('scan⟨U+202E⟩gol⟨U+2066⟩x⟨U+2069⟩ a⟨U+200D⟩b⟨U+200B⟩c⟨U+0007⟩⟨U+001B⟩[2J') &&
  out.includes('scan⟨U+202E⟩gol⟨U+2066⟩x⟨U+2069⟩ a⟨U+200D⟩b⟨U+200B⟩c⟨U+0007⟩⟨U+001B⟩[2J\n    indented\tline\n');
console.log((outOk ? 'PASS' : 'FAIL') + '  a call\'s output and peek show hidden characters as code points and keep their layout');
if(!outOk) console.log('        peek=' + JSON.stringify(peek) + ' out=' + JSON.stringify(out));
ok = outOk && ok;
// A denial's reason and a subagent's ask header are drawn the same way.
render({seq:152, type:'action.requested', payload:{call_id:'c8', tool:'bash', args:{command:'ls'}}});
render({seq:153, type:'action.denied', payload:{call_id:'c8', reason:'policy\u202eyes'}});
render({seq:154, type:'subagent.ask', payload:{request_id:'r1', tool:'bash\u200b', subagent:'sub\u202e', args:{path:'rm\u2066 x'}}});
const asked = tx.textContent;
const askOk = !raw(asked) && asked.includes('denied — policy⟨U+202E⟩yes') && asked.includes('bash⟨U+200B⟩') &&
  asked.includes('subagent sub⟨U+202E⟩') && asked.includes('rm⟨U+2066⟩ x');
console.log((askOk ? 'PASS' : 'FAIL') + '  a denial reason and a subagent ask header show hidden characters');
if(!askOk) console.log('        ' + JSON.stringify(asked));
ok = askOk && ok;
// Replies and reasoning, streamed or whole, are model text and drawn the same way, once.
for(const [label, evs] of [
  ['streamed', [{seq:160, type:'agent.delta', payload:{text:'one\u202eowt'}}, {seq:161, type:'agent.delta', payload:{text:' \u200d\u2066i\u2069\u200b\u0007\u001b'}},
    {seq:162, type:'agent.reasoning', payload:{text:'why\u2069'}}, {seq:163, type:'agent.message', payload:{text:'one\u202eowt \u200d\u2066i\u2069\u200b\u0007\u001b'}}]],
  ['whole', [{seq:170, type:'agent.reasoning', payload:{text:'why\u2069'}}, {seq:171, type:'agent.message', payload:{text:'one\u202eowt \u200d\u2066i\u2069\u200b\u0007\u001b'}}]]]){
  tx.childNodes.length = 0; turnEl = null; streamEl = null; streamBody = null;
  for(const ev of evs) render(ev);
  const said = tx.querySelectorAll('.said:not(.user)');
  const t = tx.textContent;
  const replyOk = said.length === 1 && !raw(t) && said[0].textContent.includes('one⟨U+202E⟩owt ⟨U+200D⟩⟨U+2066⟩i⟨U+2069⟩⟨U+200B⟩⟨U+0007⟩⟨U+001B⟩') && t.includes('why⟨U+2069⟩');
  console.log((replyOk ? 'PASS' : 'FAIL') + '  a ' + label + ' reply and its reasoning show hidden characters, in one bubble');
  if(!replyOk) console.log('        ' + JSON.stringify(t));
  ok = replyOk && ok;
}
// The person's message is drawn as said, with hidden characters written out: in a shared or
// automated session it is not always their own text. Newlines and indentation are kept.
tx.childNodes.length = 0; turnEl = null; streamEl = null; streamBody = null;
render({seq:180, type:'user.message', payload:{text:'fix‮txt.exe​\u001b[2J\n    indented\tline'}});
const mine = tx.querySelectorAll('.user'), mineText = mine.length ? mine[0].textContent : '';
const mineOk = mine.length === 1 && !raw(mineText) && mineText.includes('fix⟨U+202E⟩txt.exe⟨U+200B⟩⟨U+001B⟩[2J\n    indented\tline');
console.log((mineOk ? 'PASS' : 'FAIL') + '  the person\'s message shows hidden characters and keeps its layout');
if(!mineOk) console.log('        ' + JSON.stringify(mineText));
ok = mineOk && ok;
process.exit(ok?0:1);
