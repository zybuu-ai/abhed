// Every event type drawn with a payload redaction could not run on, which the
// record holds as {"withheld": ...}: nothing reads "undefined". Spliced in
// below the page's real render() by console_render_test.go.
let ok = true;
const TYPES = ['action.approved', 'action.denied', 'action.requested', 'agent.delta', 'agent.message', 'agent.reasoning',
  'compaction.completed', 'context.offloaded', 'conversation.forked', 'model.call', 'observation', 'session.ended',
  'session.woken', 'shell.ended', 'shell.started', 'subagent.action', 'subagent.ask', 'subagent.notice', 'subagent.returned',
  'subagent.spawned', 'suggestion.offered', 'todo.updated', 'user.message'];
for(const type of TYPES){
  __clear();
  let err = null;
  try{ render({seq:1, type, payload:{withheld:'[redacted: output withheld]'}, created_at:new Date().toISOString()}); }catch(e){ err = e.message; }
  const shown = __shown();
  const pass = !err && !shown.includes('undefined');
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + type + (err ? ' throws ' + err : pass ? '' : ' draws ' + shown.slice(0, 120)));
  ok = ok && pass;
}
if(!ok) process.exit(1);
