// Assertions driven against the workbench's real planCard, spliced in above
// this file's contents by plan_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const buttons = n => n.querySelectorAll('btns')[0].childNodes;
const click = async b => { await b.on.click(); await new Promise(r => setTimeout(r, 0)); };

const rb = {hash: 'ab'.repeat(32), steps: [
  {index: 1, tool: 'ssh', target: 'web1', args: {host: 'web1', command: 'nginx -t‮'}, note: 'pre-flight'},
  {index: 2, tool: 'ssh', target: 'web1', args: {host: 'web1', command: 'rm -rf /var/cache/nginx'}, destructive: 'recursive/forced delete on web1'},
]};
planCard({text: '1. rotate', hash: rb.hash, runbook: rb});
const card = __root.childNodes[0], shown = card.textContent;
check('the card lists each step with its target and command: ' + JSON.stringify(shown),
  shown.includes('Runbook · 2 steps · plan ' + rb.hash.slice(0, 12)) && shown.includes('web1') && shown.includes('rm -rf /var/cache/nginx') && shown.includes('no undo'));
check('a step\'s hidden characters are written out', shown.includes('nginx -t⟨U+202E⟩') && !shown.includes('‮'));
check('the card warns of hidden characters', shown.includes('hidden or control characters'));
check('the three answers are offered', buttons(card).length === 3);

// A step with no undo is asked about again before anything is sent.
await click(buttons(card)[1]);
check('approving first asks about the steps with no undo', __posts.length === 0 && card.textContent.includes('cannot be undone'));
await click(buttons(card)[1]);
check('confirming sends the answer with confirm_no_undo: ' + JSON.stringify(__posts),
  __posts.length === 1 && __posts[0].decision === 'approve' && __posts[0].plan_hash === rb.hash && __posts[0].confirm_no_undo === true);
check('the conversation goes on with the message the server returned', __sent.length === 1 && __sent[0] === 'carry it out');
check('the card is settled', !card.querySelector('btns') && card.textContent.includes('approved'));

// Keeping planning sends that, and nothing else.
planCard({text: '2. again', hash: 'cd'.repeat(32)});
const second = __root.childNodes[1];
await click(buttons(second)[2]);
check('keep planning is sent as such', __posts.length === 2 && __posts[1].decision === 'keep-planning' && __sent.length === 1);

if(!ok) process.exit(1);
