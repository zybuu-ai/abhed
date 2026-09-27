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
process.exit(ok?0:1);
