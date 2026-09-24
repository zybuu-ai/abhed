package server

import "net/http"

// serveAccount lets a local-accounts user change their own password, which
// is also the only way out of a password an administrator set for them.
func (s *Server) serveAccount(w http.ResponseWriter, r *http.Request) {
	if s.LocalAuth() == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	_, _ = w.Write([]byte(accountHTML))
}

const accountHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Account · Abhed</title><link rel="icon" href="/favicon.svg" type="image/svg+xml">
<style>
:root{color-scheme:light dark;--bg:#0f1115;--fg:#e6e8ee;--mut:#9aa3b2;--line:#262b36;--acc:#2A8CF0;--bad:#f06a6a;--ok:#3fbf7f}
@media (prefers-color-scheme:light){:root{--bg:#f7f8fa;--fg:#1f2430;--mut:#5b6474;--line:#dde1e8}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,Segoe UI,sans-serif;display:grid;place-items:center;min-height:100vh;padding:16px;box-sizing:border-box}
main{width:100%;max-width:380px}h1{font-size:20px;margin:0 0 4px}p{color:var(--mut);margin:0 0 20px}
label{display:block;font-size:13px;color:var(--mut);margin:12px 0 4px}
input{width:100%;box-sizing:border-box;padding:9px 10px;border:1px solid var(--line);border-radius:6px;background:transparent;color:inherit;font:inherit}
button{margin-top:18px;width:100%;padding:10px;border:0;border-radius:6px;background:var(--acc);color:#fff;font:inherit;cursor:pointer}
button:disabled{opacity:.6;cursor:default}#msg{margin-top:14px;min-height:1.5em}.bad{color:var(--bad)}.ok{color:var(--ok)}
nav{margin-top:22px;display:flex;gap:16px}a{color:var(--acc)}
</style></head><body><main>
<h1>Change password</h1><p id="who">Signed in.</p>
<p id="must" class="bad" hidden>This password was set for you. Change it to continue.</p>
<form id="f">
<label for="cur">Current password</label><input id="cur" type="password" autocomplete="current-password" required>
<label for="nw">New password (at least 10 characters)</label><input id="nw" type="password" autocomplete="new-password" minlength="10" required>
<label for="nw2">New password, again</label><input id="nw2" type="password" autocomplete="new-password" minlength="10" required>
<button id="go" type="submit">Change password</button><div id="msg" role="status"></div>
</form>
<nav><a href="/ide">Back to the workbench</a><a href="/logout">Sign out</a></nav>
</main><script>
const $ = (id) => document.getElementById(id);
try{ if(new URLSearchParams(location.search).get('must_change')) $('must').hidden = false; }catch{}
fetch('/v1/whoami').then(r => r.json()).then(me => {
  if(!me.authenticated){ location.href = '/'; return; }
  if(!me.password_url){ document.querySelector('main').textContent = 'This account signs in through your identity provider; change its password there.'; return; }
  $('who').textContent = 'Signed in as ' + (me.email || me.name || me.subject) + '.';
  if(me.must_change_password) $('must').hidden = false;
}).catch(() => {});
$('f').addEventListener('submit', async (e) => {
  e.preventDefault();
  const msg = $('msg'); msg.className = ''; msg.textContent = '';
  if($('nw').value !== $('nw2').value){ msg.className = 'bad'; msg.textContent = 'The new passwords do not match.'; return; }
  $('go').disabled = true;
  try{
    const r = await fetch('/v1/password', {method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({current_password: $('cur').value, new_password: $('nw').value})});
    let body = {}; try{ body = await r.json(); }catch{}
    if(r.ok){ msg.className = 'ok'; msg.textContent = 'Password changed.'; $('f').reset(); $('must').hidden = true; }
    else{ msg.className = 'bad'; msg.textContent = body.error || ('Could not change it (' + r.status + ').'); }
  }catch{ msg.className = 'bad'; msg.textContent = 'Cannot reach the server.'; }
  $('go').disabled = false;
});
</script></body></html>`
