// Assertions driven against ollama_card.js, spliced in after it by
// ollama_card_test.go. fetch, confirm and the page are fakes.
let ok = true;
function check(label, pass){ console.log((pass ? 'PASS' : 'FAIL') + '  ' + label); ok = ok && pass; }
const text = n => n ? n.textContent : '';
const buttonsOf = card => { const out = []; const walk = n => { for(const c of n.childNodes){ if(c.tag === 'button') out.push(c); if(c.childNodes) walk(c); } }; walk(card); return out; };
const button = (card, label) => buttonsOf(card).find(b => b.textContent.startsWith(label));

const notReady = {provider:'local', model:'qwen3:8b', running:false, installed:true, ready:false, next:'start', can_act:true,
  problem:{kind:'not_running', message:'Ollama is installed but not running at http://127.0.0.1:11434.', steps:['ollama serve', 'ollama pull qwen3:8b']},
  start:{command:'OLLAMA_HOST=127.0.0.1:11434 /usr/local/bin/ollama serve'}, catalog:[]};
globalThis.__replies = {'/v1/ollama/status?provider=local': [notReady]};
const card = {card:null, provider:'local', session:'s1', mount: n => __root.appendChild(n)};
await abhedOllama.show(card);
check('a card is drawn for a model that is not ready', card.card && text(card.card).includes('Ollama is installed but not running'));
check('the steps are listed', text(card.card).includes('ollama serve') && text(card.card).includes('ollama pull qwen3:8b'));
check('the exact command is shown', text(card.card).includes('OLLAMA_HOST=127.0.0.1:11434 /usr/local/bin/ollama serve'));

// Declining the confirm sends nothing.
__confirm = false;
button(card.card, 'Start Ollama').onclick();
await new Promise(r => setTimeout(r, 0));
check('nothing is sent without a yes', __posted.length === 0);
check('the confirm shows the command', __asked[0].includes('/usr/local/bin/ollama serve'));

// A yes starts it, for the session, and the card goes once the model is ready.
__confirm = true;
__replies['/v1/ollama/start'] = [{started:{pid:1, version:'0.40.10'}, status:{ready:true}}];
__replies['/v1/ollama/status?provider=local'] = [{ready:true}];
button(card.card, 'Start Ollama').onclick();
await new Promise(r => setTimeout(r, 10));
check('a yes posts the start for the session', __posted.length === 1 && __posted[0].url === '/v1/ollama/start' && __posted[0].body.session_id === 's1' && !__posted[0].body.always);
check('the card goes once the model is ready', card.card === null);

// A refusal is shown with its steps, as text.
const refused = Object.assign({}, notReady, {can_act:false, start:undefined, refusal:{kind:'not_admin', message:'Only an administrator of this server can start, install or download Ollama on it.', steps:[]}});
__replies['/v1/ollama/status?provider=local'] = [refused];
await abhedOllama.show(card);
check('a person who may not act sees why and no action', text(card.card).includes('Only an administrator') && !button(card.card, 'Start Ollama') && button(card.card, 'Check again'));

// A pull shows its size and streams progress; a failure shows the problem.
const pull = Object.assign({}, notReady, {next:'pull', start:undefined, pull:{command:'ollama pull qwen3:8b', source:'https://registry.ollama.ai', bytes:5225388164, disk_free:100e9, warnings:['qwen3:8b wants about 8 GB of memory']}});
__replies['/v1/ollama/status?provider=local'] = [pull, {ready:true}];
__replies['/v1/ollama/status?provider=local&detail=1'] = [pull];
__stream = ['{"progress":{"status":"downloading","completed":50,"total":100}}', '{"status":{"ready":true}}'];
__fetched.length = 0;
await abhedOllama.show(card);
check('the pull shows its source, size from the list and warnings', text(card.card).includes('5.2 GB') && text(card.card).includes('registry.ollama.ai') && text(card.card).includes('8 GB of memory'));
check('showing the card asks for no size', !__fetched.some(u => u.includes('detail=1')));
__asked.length = 0;
await button(card.card, 'Download').onclick();
await new Promise(r => setTimeout(r, 10));
check('opening the download asks for the size', __fetched.some(u => u.includes('detail=1')));
check('the pull confirm names the size', __asked[0] && __asked[0].includes('5.2 GB'));
check('the pull is posted', __posted.some(p => p.url === '/v1/ollama/pull'));
check('the card goes after a pull that ends ready', card.card === null);

__replies['/v1/ollama/status?provider=local'] = [pull];
__replies['/v1/ollama/status?provider=local&detail=1'] = [pull];
__stream = ['{"problem":{"kind":"disk_short","message":"Not enough disk <b>space</b>","steps":["free 20 GB"]}}'];
await abhedOllama.show(card);
await button(card.card, 'Download').onclick();
await new Promise(r => setTimeout(r, 10));
check('a failed pull shows the problem as text', text(card.card).includes('Not enough disk <b>space</b>') && text(card.card).includes('free 20 GB'));

if(!ok) process.exit(1);
