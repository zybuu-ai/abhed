// The Ollama card, shared by the workbench and the console: for a chosen
// model a local Ollama serves, what is wrong, the numbered steps a person can
// run, and the next step when this person may take it. Every action is
// confirmed first, showing its exact command, source and size; every text
// from the server is shown as text.
const abhedOllama = (() => {
  let styled = false;
  function style(){
    if(styled) return; styled = true;
    const s = document.createElement('style');
    s.textContent = '.olc{margin:8px 12px;padding:10px 12px;border:1px solid var(--line,#444);border-left:3px solid var(--warn,#c80);border-radius:6px;font-size:12px}' +
      '.olc b{font-family:var(--mono,monospace)}.olc p{margin:4px 0;overflow-wrap:anywhere}.olc .warn{color:var(--warn,#c80)}' +
      '.olc ol{margin:4px 0 4px 18px;padding:0}.olc li code,.olc p code{font-family:var(--mono,monospace);overflow-wrap:anywhere}' +
      '.olc .row{display:flex;gap:6px;flex-wrap:wrap;margin-top:6px}.olc button{font:inherit;padding:3px 9px;border-radius:5px;border:1px solid var(--line,#444);background:none;color:inherit;cursor:pointer}' +
      '.olc button.go{border-color:var(--accent,#48f)}.olc button:disabled{opacity:.5;cursor:default}.olc .res{color:var(--muted,#888)}';
    document.head.appendChild(s);
  }
  function mk(tag, cls, text){ const n = document.createElement(tag); if(cls) n.className = cls; if(text != null) n.textContent = text; return n; }
  function size(n){
    if(!n) return 'unknown';
    if(n >= 1e9) return (n / 1e9).toFixed(1) + ' GB';
    if(n >= 1e6) return Math.round(n / 1e6) + ' MB';
    return Math.round(n / 1e3) + ' KB';
  }
  async function call(path, opts){
    let r;
    try{ r = await fetch(path, Object.assign({headers:{'Content-Type':'application/json'}, credentials:'same-origin'}, opts || {})); }
    catch{ return {ok:false, body:{error:'the server could not be reached'}}; }
    let b = {}; try{ b = (await r.json()) || {}; }catch{}
    return {ok: r.ok, status: r.status, body: b};
  }
  function steps(card, list){
    if(!list || !list.length) return;
    card.appendChild(mk('p', '', 'To do it yourself:'));
    const ol = mk('ol');
    for(const s of list){ const li = mk('li'); li.appendChild(mk('code', '', s)); ol.appendChild(li); }
    card.appendChild(ol);
  }
  function problem(res, p, fallback){
    res.textContent = '';
    res.className = 'res warn';
    res.appendChild(mk('span', '', (p && p.message) || fallback || 'That did not work.'));
    if(p && p.steps && p.steps.length){
      const ol = mk('ol');
      for(const s of p.steps){ const li = mk('li'); li.appendChild(mk('code', '', s)); ol.appendChild(li); }
      res.appendChild(ol);
    }
  }
  // show draws the card for o.provider, replacing o's earlier one; o.mount
  // places a new card, o.session is the session whose record takes the attempt.
  async function show(o){
    style();
    const r = await call('/v1/ollama/status?provider=' + encodeURIComponent(o.provider));
    if(!r.ok) return;
    const rep = r.body;
    if(rep.ready){ if(o.card){ o.card.remove(); o.card = null; } return; }
    const card = mk('div', 'olc');
    const head = mk('p'); head.appendChild(mk('b', '', 'Ollama · ' + o.provider + (rep.model ? ' (' + rep.model + ')' : ''))); card.appendChild(head);
    if(rep.problem) card.appendChild(mk('p', '', rep.problem.message));
    if(rep.refusal) card.appendChild(mk('p', 'warn', rep.refusal.message));
    steps(card, rep.problem && rep.problem.steps);
    const res = mk('p', 'res'), row = mk('div', 'row');
    const body = extra => JSON.stringify(Object.assign({provider: o.provider, session_id: o.session || undefined}, extra || {}));
    const buttons = [];
    const button = (label, cls, fn) => { const b = mk('button', cls, label); b.type = 'button'; b.addEventListener('click', fn); row.appendChild(b); buttons.push(b); return b; };
    const busy = on => { for(const b of buttons) b.disabled = on; };
    async function act(path, extra){
      busy(true); res.className = 'res'; res.textContent = 'working…';
      const a = await call(path, {method:'POST', body: body(extra)});
      busy(false);
      if(!a.ok){ problem(res, a.body.problem, a.body.error); return; }
      if(a.body.outcome === 'manual'){ res.textContent = 'The download page is open on the server host. Install Ollama there, then check again.'; return; }
      show(o);
    }
    if(rep.can_act && rep.start){
      card.appendChild(mk('p', '', 'Start command: ')).appendChild(mk('code', '', rep.start.command));
      card.appendChild(mk('p', 'res', rep.start.app_binds
        ? 'This opens the Ollama app on the server host; the app listens where its own settings say, which Abhed does not control. It keeps running when Abhed stops.'
        : 'It runs on the server host only, listening on loopback, as the server\'s user, and keeps running when Abhed stops.'));
      button('Start Ollama', 'go', () => {
        if(confirm('Start Ollama on the server host?\n\nCommand: ' + rep.start.command + '\n\nIt keeps running when Abhed stops.')) act('/v1/ollama/start');
      });
      if(!rep.start_always) button('Always start without asking', '', () => {
        if(confirm('Start Ollama now, and from now on without asking?\n\nCommand: ' + rep.start.command + '\n\nAn administrator takes this back with abhed ollama forget, or POST /v1/ollama/forget.')) act('/v1/ollama/start', {always: true});
      });
    } else if(rep.can_act && rep.install){
      const p = rep.install;
      card.appendChild(mk('p', '', 'Install command: ')).appendChild(mk('code', '', p.command));
      card.appendChild(mk('p', 'res', 'Source: ' + p.source + ' · size: ' + (p.size || 'not known until the download starts')));
      button(p.kind === 'open' ? 'Open the download page' : 'Install Ollama', 'go', () => {
        if(confirm('Install Ollama on the server host?\n\nCommand: ' + p.command + '\nSource: ' + p.source + '\nSize: ' + (p.size || 'not known until the download starts') + '\n\nIt runs as the server\'s user, without sudo.')) act('/v1/ollama/install');
      });
    } else if(rep.can_act && rep.pull){
      const p = rep.pull;
      card.appendChild(mk('p', '', 'Download command: ')).appendChild(mk('code', '', p.command));
      card.appendChild(mk('p', 'res', 'Source: ' + p.source + ' · size: ' + size(p.bytes) + (p.disk_free ? ' · free on the host: ' + size(p.disk_free) : '')));
      for(const w of (p.warnings || [])) card.appendChild(mk('p', 'warn', w));
      let ctl = null;
      const stop = mk('button', '', 'Stop'); stop.type = 'button'; stop.hidden = true;
      button('Download ' + size(p.bytes), 'go', async () => {
        if(!confirm('Download ' + rep.model + ' to the server host?\n\nCommand: ' + p.command + '\nSource: ' + p.source + '\nSize: ' + size(p.bytes) + (p.warnings && p.warnings.length ? '\n\n' + p.warnings.join('\n') : ''))) return;
        ctl = new AbortController(); busy(true); stop.hidden = false; res.className = 'res'; res.textContent = 'starting the download…';
        let r2;
        try{ r2 = await fetch('/v1/ollama/pull', {method:'POST', headers:{'Content-Type':'application/json'}, credentials:'same-origin', body: body(), signal: ctl.signal}); }
        catch{ busy(false); stop.hidden = true; res.textContent = 'The download stopped. Ollama keeps what it fetched; downloading again resumes it.'; return; }
        if(!r2.ok){ let b = {}; try{ b = await r2.json(); }catch{} busy(false); stop.hidden = true; problem(res, b.problem, b.error); return; }
        const rd = r2.body.getReader(), dec = new TextDecoder(); let buf = '', done = false;
        try{
          while(!done){
            const c = await rd.read(); if(c.done) break;
            buf += dec.decode(c.value, {stream:true});
            let i;
            while((i = buf.indexOf('\n')) >= 0){
              const line = buf.slice(0, i); buf = buf.slice(i + 1);
              let m; try{ m = JSON.parse(line); }catch{ continue; }
              if(m.progress){ const g = m.progress; res.textContent = g.status + (g.total ? ' ' + Math.floor(g.completed * 100 / g.total) + '% of ' + size(g.total) : ''); }
              if(m.problem){ problem(res, m.problem); done = true; }
              if(m.status){ done = true; show(o); }
            }
          }
        }catch{ res.textContent = 'The download stopped. Ollama keeps what it fetched; downloading again resumes it.'; }
        busy(false); stop.hidden = true;
      });
      stop.addEventListener('click', () => { if(ctl) ctl.abort(); });
      row.appendChild(stop);
    }
    button('Check again', '', () => show(o));
    card.appendChild(row); card.appendChild(res);
    if(o.card && o.card.isConnected) o.card.replaceWith(card); else o.mount(card);
    o.card = card;
  }
  return {show};
})();
