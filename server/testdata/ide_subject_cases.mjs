// Assertions driven against the workbench's real subjectOf(), spliced in
// above this file's contents by ide_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
check('a web_fetch call is shown by its URL', subjectOf('web_fetch', {url:'https://example.com/'}) === 'https://example.com/');
check('a command is still shown as itself', subjectOf('bash', {command:'ls -la'}) === 'ls -la');
check('arguments with nothing to name are shown as JSON', subjectOf('x', {a:1}) === '{"a":1}');
check('a skill call is shown by the skill it runs', subjectOf('skill', {name:'release-notes'}) === 'release-notes');
check('a tasks call names its subagents', subjectOf('tasks', {tasks:[{description:'scan api', prompt:'p'}, {agent_type:'reviewer', prompt:'q'}]}) === '2 subagents: scan api, reviewer');
if(!ok) process.exit(1);
