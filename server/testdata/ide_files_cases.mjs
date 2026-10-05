// Assertions driven against the workbench's downloads, session title, delete,
// resume command and notifications, spliced in above this file by ide_files_test.go.
let ok = true;
function check(label, pass){
  console.log((pass ? 'PASS' : 'FAIL') + '  ' + label);
  ok = ok && pass;
}
const tick = (ms = 5) => new Promise(r => setTimeout(r, ms));
const RLO = String.fromCharCode(0x202e);

// The Files list draws names as text with hidden characters written out, and a
// click downloads through the route only after it answers.
__files = [{name:'report' + RLO + 'fdp.docx', bytes:2048}, {name:'plan.pdf', bytes:10}];
await loadDownloads();
const rows = $('dl').querySelectorAll('row');
check('each file is a row', rows.length === 2);
check('a hidden character in a name is shown as its code point', rows[0].textContent.includes('report⟨U+202E⟩fdp.docx') && !$('dl').textContent.includes(RLO));
check('sizes are shown', rows[0].textContent.includes('2.0 KB') && rows[1].textContent.includes('10 B'));
rows[1].on.click(); await tick();
check('a download asks the route first, by HEAD', __heads.length === 1 && __heads[0] === '/v1/sessions/s1/download?path=plan.pdf');
check('then the browser saves it as a download', __clicked.length === 1 && __clicked[0].href === __heads[0] && __clicked[0].download === '');
__headStatus = 404; rows[0].on.click(); await tick();
check('a refused download is said, not saved', __clicked.length === 1 && $('dl').textContent.includes('is not there, or a rule keeps it'));
check('and its name is shown with the hidden character written out', $('dl').textContent.includes('report⟨U+202E⟩fdp.docx is not there'));
__files = []; await loadDownloads();
check('with nothing to offer, the list says where any file can be downloaded', $('dl').textContent.includes('right-click it'));

// A title is preferred to the prompt, and both are revealed.
check('the title names the session', sessionLabel({title:'Fix' + RLO + 'x', prompt:'p'}) === 'Fix⟨U+202E⟩x');
check('the prompt names it with no title', sessionLabel({prompt:'do it'}) === 'do it');
check('a session with neither is a workbench session', sessionLabel({}) === 'Workbench session' && sessionLabel({prompt:'  \n '}) === 'Workbench session');
check('a prompt names it by its first line, revealed', sessionLabel({prompt:'\n  what is ' + RLO + 'this?\n\nAttached file:\n- /w/uploads/x.png'}) === 'what is ⟨U+202E⟩this?');

// Renaming: Enter sends the trimmed title, Escape sends nothing.
const s = {id:'s1', prompt:'opening prompt'}, span = new El('span');
editTitle(span, s);
let inp = __focused; inp.value = '  Retry fix  '; inp.on.keydown({key:'Enter', preventDefault(){}, stopPropagation(){}}); await tick();
check('Enter posts the trimmed title to the title route', __posts.length === 1 && __posts[0].url === '/v1/sessions/s1/title' && __posts[0].body.title === 'Retry fix');
check('the title is shown once the server records it', s.title === 'Retry fix' && span.textContent === 'Retry fix');
editTitle(span, s); inp = __focused; inp.value = 'not this'; inp.on.keydown({key:'Escape', preventDefault(){}, stopPropagation(){}}); await tick();
check('Escape renames nothing', __posts.length === 1 && span.textContent === 'Retry fix');
__titleError = 'the session is mid-turn; rename it once the turn has finished';
editTitle(span, s); inp = __focused; inp.value = 'Later'; inp.on.blur(); await tick();
check('a refused rename is said and the title stays', s.title === 'Retry fix' && __notes.some(n => n.includes('Not renamed: the session is mid-turn')));

// Delete asks first, naming the session revealed, and says what delete means here.
__confirm = false; await deleteSession({id:'s2', title:'Old' + RLO + 'one'});
check('declined: nothing is deleted', !__deletes.length);
check('the question names the session with the hidden character written out', __asked[0].includes('"Old⟨U+202E⟩one"') && !__asked[0].includes(RLO));
check('and says the database keeps its events', __asked[0].includes('events stay in the database for audit'));
__confirm = true; await deleteSession({id:'s2'});
check('confirmed: the session is deleted', __deletes.length === 1 && __deletes[0] === '/v1/sessions/s2');
__deleteStatus = 501; await deleteSession({id:'s3'});
check('an append-only store is named as the reason', __notes.some(n => n.includes('append-only record')));
__deleteStatus = 0; current = 's4'; await deleteSession({id:'s4'});
check('deleting the open session closes it', __reset === 1);

// The resume command is offered only for a plain id, never one that would run something else.
check('a plain id', resumeCommand('s-abc_1') === 'abhed -r s-abc_1');
for(const bad of ['s1; rm -rf ~', 's1 && x', '$(x)', '', 's1\nx', '../s1']) check('refused: ' + JSON.stringify(bad), resumeCommand(bad) === '');
await copyResume('s-abc_1');
check('the command is copied', __clipboard === 'abhed -r s-abc_1');

// Export saves the transcript or the record through the export route, for a plain id only.
__clicked.length = 0; exportSession('s-abc_1', 'html'); exportSession('s-abc_1', 'jsonl'); exportSession('s1; x', 'html');
check('export downloads the transcript and the record', __clicked.length === 2 && __clicked[0].href === '/v1/sessions/s-abc_1/export?format=html' && __clicked[1].href === '/v1/sessions/s-abc_1/export?format=jsonl' && __clicked[0].download === '');

// Notifications: off until the person turns them on, only while hidden, and never with record text.
check('no notification before opting in', (notify('ask'), __shown.length === 0));
await toggleNotify();
check('turning them on asks the browser once and remembers it', __permissionAsks === 1 && __store.notify === '1' && $('s-notify').textContent === 'notifications on');
document.hidden = false; notify('ask');
check('none while the tab is in front', __shown.length === 0);
document.hidden = true; notify('ask'); notify('done'); notify('rm -rf ' + RLO);
check('an ask and an end each notify, with fixed text', __shown.length === 2 && __shown[0].body === 'An approval is waiting in Abhed.' && __shown[1].body === 'A run has finished in Abhed.');
check('nothing but the fixed kinds is shown', __shown.every(n => n.title === 'Abhed' && !n.body.includes('rm')));
await toggleNotify();
notify('ask');
check('turned off, nothing more is shown', __shown.length === 2 && $('s-notify').textContent === 'notifications off');

if(!ok) process.exit(1);
