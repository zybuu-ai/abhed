package server

import (
	"net/http"

	"github.com/zybuu-ai/abhed/auth"
)

// CLIDiscoveryPath is where `abhed login` learns how this server signs a
// command line in. It is public: it says what the sign-in page would.
const CLIDiscoveryPath = "/v1/cli"

// cliRoutes registers command-line sign-in when it is configured, and the
// discovery document always, so a client can tell "no sign-in needed" from
// "not offered here".
func (s *Server) cliRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+CLIDiscoveryPath, s.cliDiscovery)
	mux.HandleFunc("GET /v1/cli/whoami", s.cliWhoami)
	if l := s.opts.CLILogin; l != nil {
		l.Routes(mux)
		mux.HandleFunc("GET "+auth.CLIPagePath, s.serveCLILogin)
	}
}

func (s *Server) cliDiscovery(w http.ResponseWriter, r *http.Request) {
	methods := []auth.CLILoginMethod{}
	if l := s.opts.CLILogin; l != nil {
		methods = append(methods, l.Method())
	}
	for _, p := range s.signIns() {
		if a, ok := p.(auth.CLILoginAdvertiser); ok {
			methods = append(methods, a.CLILoginMethods()...)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, http.StatusOK, map[string]any{
		"auth_mode": orDefaultStr(s.opts.Config.Auth.Mode, "none"),
		"methods":   methods,
	})
}

// cliWhoami answers for the caller as authenticated, bearer token included,
// so a command line can confirm what it holds.
func (s *Server) cliWhoami(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	if id == nil {
		WriteError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"subject": id.Subject, "name": id.Name, "email": id.Email, "tenant": id.Tenant,
		"provider": id.Provider, "owner": id.Owner(), "expires": id.Expires,
	})
}

func (s *Server) serveCLILogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	_, _ = w.Write([]byte(cliLoginHTML))
}

var cliLoginHTML = brandify(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Command-line sign-in · Abhed</title><link rel="icon" type="image/png" href="/favicon.ico">
<style>
:root{color-scheme:light dark;--bg:#0B0B0C;--fg:#F2F2EE;--mut:#9B9BA3;--line:#2A2A2F;--acc:#FF7A45;--acc-ink:#0B0B0C;--bad:#f06a6a;--ok:#3fbf7f}
@media (prefers-color-scheme:light){:root{--bg:#FAFAF8;--fg:#0B0B0C;--mut:#6B6B72;--line:#E5E5E0;--acc:#C2410C;--acc-ink:#fff}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,Segoe UI,sans-serif;display:grid;place-items:center;min-height:100vh;padding:16px;box-sizing:border-box}
main{width:100%;max-width:420px}h1{font-size:20px;margin:0 0 4px}p{color:var(--mut);margin:0 0 16px}
label{display:block;font-size:13px;color:var(--mut);margin:12px 0 4px}
input{width:100%;box-sizing:border-box;padding:9px 10px;border:1px solid var(--line);border-radius:6px;background:transparent;color:inherit;font:inherit;letter-spacing:.12em;text-transform:uppercase}
dl{display:grid;grid-template-columns:auto 1fr;gap:4px 12px;margin:0 0 16px;font-size:14px}dt{color:var(--mut)}dd{margin:0;word-break:break-all}
.row{display:flex;gap:10px;margin-top:18px}button{flex:1;padding:10px;border:0;border-radius:6px;background:var(--acc);color:var(--acc-ink);font:inherit;font-weight:600;cursor:pointer}
button.no{background:transparent;color:var(--fg);border:1px solid var(--line)}button:disabled{opacity:.6;cursor:default}
#msg{margin-top:14px;min-height:1.5em}.bad{color:var(--bad)}.ok{color:var(--ok)}.warn{color:var(--bad);font-size:14px}
/*{{BRAND_CSS}}*/
</style></head><body><main>
<div style="margin-bottom:22px">{{BRAND_LOCKUP}}</div><h1>Sign in a command line</h1>
<p id="who">Checking who you are…</p>
<section id="device" hidden>
<p>Enter the code your terminal shows. Approve only a sign-in you started yourself: whoever holds that terminal will act as you.</p>
<label for="code">Code</label><input id="code" autocomplete="off" spellcheck="false" maxlength="9" placeholder="XXXX-XXXX">
<dl id="info" hidden><dt>Client</dt><dd id="client"></dd><dt>From</dt><dd id="from"></dd><dt>Requested</dt><dd id="when"></dd></dl>
<div class="row"><button id="yes" disabled>Approve</button><button id="no" class="no" disabled>Deny</button></div>
</section>
<section id="pkce" hidden>
<p>A command line on this computer asks to sign in as you. Approve only if you just ran <code>abhed login</code> here.</p>
<dl><dt>Returns to</dt><dd id="redir"></dd></dl>
<div class="row"><button id="pyes">Approve</button><button id="pno" class="no">Deny</button></div>
</section>
<div id="msg" role="status"></div>
</main><script>
const $ = (id) => document.getElementById(id);
const q = new URLSearchParams(location.search);
const say = (t, cls) => { $('msg').textContent = t; $('msg').className = cls || ''; };
async function post(path, body){
  const r = await fetch(path, {method:'POST', headers:{'Content-Type':'application/json','Accept':'application/json'}, body: JSON.stringify(body)});
  let b = {}; try{ b = await r.json(); }catch{}
  if(!r.ok) throw new Error(b.error || ('refused (' + r.status + ')'));
  return b;
}
fetch('/v1/whoami').then(r => r.json()).then(me => {
  $('who').textContent = me.authenticated ? ('Signed in as ' + (me.name || me.subject) + '.') : 'Not signed in.';
}).catch(() => {});
if(q.get('code_challenge')){
  $('pkce').hidden = false;
  $('redir').textContent = q.get('redirect_uri') || '';
  const go = async (approve) => {
    $('pyes').disabled = $('pno').disabled = true;
    try{
      const b = await post('/v1/cli/authorize', {code_challenge:q.get('code_challenge'), code_challenge_method:q.get('code_challenge_method'),
        redirect_uri:q.get('redirect_uri'), state:q.get('state') || '', approve});
      say(approve ? 'Approved. Returning to your terminal…' : 'Denied.', approve ? 'ok' : '');
      location.href = b.redirect;
    }catch(e){ say(e.message, 'bad'); }
  };
  $('pyes').onclick = () => go(true); $('pno').onclick = () => go(false);
} else {
  $('device').hidden = false;
  let current = '';
  const look = async () => {
    const v = $('code').value.trim();
    $('info').hidden = true; $('yes').disabled = $('no').disabled = true; current = '';
    if(v.replace(/[-\s]/g,'').length !== 8) return;
    try{
      const r = await fetch('/v1/cli/approve?user_code=' + encodeURIComponent(v));
      const b = await r.json();
      if(!r.ok){ say(b.error || 'not found', 'bad'); return; }
      $('client').textContent = b.client_name; $('from').textContent = b.from;
      $('when').textContent = new Date(b.requested_at).toLocaleString();
      $('info').hidden = false; $('yes').disabled = $('no').disabled = false; current = b.user_code; say('');
    }catch(e){ say(e.message, 'bad'); }
  };
  const answer = async (approve) => {
    $('yes').disabled = $('no').disabled = true;
    try{
      await post('/v1/cli/approve', {user_code: current, approve});
      say(approve ? 'Approved. Your terminal is signed in; you can close this tab.' : 'Denied. The terminal was not signed in.', approve ? 'ok' : '');
    }catch(e){ say(e.message, 'bad'); }
  };
  $('code').oninput = look; $('yes').onclick = () => answer(true); $('no').onclick = () => answer(false);
  if(q.get('user_code')){ $('code').value = q.get('user_code'); look(); }
}
</script></body></html>`)
