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
process.exit(ok?0:1);
