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

var accountHTML = brandify(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Account · Abhed</title><link rel="icon" type="image/png" href="/favicon.ico">
<style>
:root{color-scheme:light dark;--bg:#0B0B0C;--fg:#F2F2EE;--mut:#9B9BA3;--line:#2A2A2F;--acc:#FF7A45;--acc-ink:#0B0B0C;--bad:#f06a6a;--ok:#3fbf7f}
@media (prefers-color-scheme:light){:root{--bg:#FAFAF8;--fg:#0B0B0C;--mut:#6B6B72;--line:#E5E5E0;--acc:#C2410C;--acc-ink:#fff}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,Segoe UI,sans-serif;display:grid;place-items:center;min-height:100vh;padding:16px;box-sizing:border-box}
main{width:100%;max-width:380px}h1{font-size:20px;margin:0 0 4px}p{color:var(--mut);margin:0 0 20px}
label{display:block;font-size:13px;color:var(--mut);margin:12px 0 4px}
input{width:100%;box-sizing:border-box;padding:9px 10px;border:1px solid var(--line);border-radius:6px;background:transparent;color:inherit;font:inherit}
button{margin-top:18px;width:100%;padding:10px;border:0;border-radius:6px;background:var(--acc);color:var(--acc-ink);font:inherit;font-weight:600;cursor:pointer}
button:disabled{opacity:.6;cursor:default}#msg{margin-top:14px;min-height:1.5em}.bad{color:var(--bad)}.ok{color:var(--ok)}
nav{margin-top:22px;display:flex;gap:16px}a{color:var(--acc)}
nav form{margin:0}button.link{margin:0;width:auto;padding:0;background:none;color:var(--acc);font-weight:400;text-decoration:underline}
/*{{BRAND_CSS}}*/
</style></head><body><main>
<div style="margin-bottom:22px">{{BRAND_LOCKUP}}</div><h1>Change password</h1><p id="who">Signed in.</p>
<p id="must" class="bad" hidden>This password was set for you. Change it to continue.</p>
<form id="f">
<label for="cur">Current password</label><input id="cur" type="password" autocomplete="current-password" required>
<label for="nw">New password (at least 10 characters)</label><input id="nw" type="password" autocomplete="new-password" minlength="10" required>
<label for="nw2">New password, again</label><input id="nw2" type="password" autocomplete="new-password" minlength="10" required>
<button id="go" type="submit">Change password</button><div id="msg" role="status"></div>
</form>
<nav><a href="/ide">Back to the workbench</a><form method="post" action="/logout"><button class="link" type="submit">Sign out</button></form></nav>
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
</script></body></html>`)

// serveSignOut asks before signing out. Signing out is a POST: a GET that
// ended the session let any page sign a person out with a link or an image.
func (s *Server) serveSignOut(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	_, _ = w.Write([]byte(signOutHTML))
}

var signOutHTML = brandify(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Sign out · Abhed</title><link rel="icon" type="image/png" href="/favicon.ico">
<style>
:root{color-scheme:light dark;--bg:#0B0B0C;--fg:#F2F2EE;--mut:#9B9BA3;--acc:#FF7A45;--acc-ink:#0B0B0C}
@media (prefers-color-scheme:light){:root{--bg:#FAFAF8;--fg:#0B0B0C;--mut:#6B6B72;--acc:#C2410C;--acc-ink:#fff}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,Segoe UI,sans-serif;display:grid;place-items:center;min-height:100vh;padding:16px;box-sizing:border-box}
main{width:100%;max-width:380px}h1{font-size:20px;margin:0 0 4px}p{color:var(--mut);margin:0 0 20px}
button{width:100%;padding:10px;border:0;border-radius:6px;background:var(--acc);color:var(--acc-ink);font:inherit;font-weight:600;cursor:pointer}
a{color:var(--acc)}nav{margin-top:22px}
/*{{BRAND_CSS}}*/
</style></head><body><main>
<div style="margin-bottom:22px">{{BRAND_LOCKUP}}</div><h1>Sign out</h1><p>End your session on this browser.</p>
<form method="post" action="/logout"><button type="submit" autofocus>Sign out</button></form>
<nav><a href="/ide">Back to the workbench</a></nav>
</main></body></html>`)
