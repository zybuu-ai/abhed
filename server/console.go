package server

import (
	"html"
	"net/http"
	"net/url"
	"strings"
)

// serveConsole serves the web console.
//
// One self-contained HTML document with no external requests: an air-gapped
// enclave has no CDN, and a build step would mean the binary alone is not a
// complete install. Everything here is hand-written CSS and vanilla JS against
// the same SSE stream the CLI consumes.
//
// The layout is an operator console, not a chat window: a session rail, the
// transcript, and a run inspector carrying the numbers that matter on owned
// GPUs — turns, tokens, cache hit rate, tool tallies, terminal reason.
func (s *Server) serveConsole(w http.ResponseWriter, r *http.Request) {
	// The console moved to /console when / became the landing page; this guard
	// still checked for "/" and 404'd its own route.
	if r.URL.Path != "/console" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.Write([]byte(WithHome(consoleHTML, s.opts.HomeURL)))
}

// WithHome substitutes the operator's site link, or removes the placeholder
// when none is configured. The link's text is the site's host: the page
// cannot know what the operator calls their site, and a hostname is what a
// person expects a "back to" link to say.
func WithHome(page, home string) string {
	if home == "" {
		return strings.ReplaceAll(page, "<!--HOME-->", "")
	}
	esc := html.EscapeString(home)
	label := esc
	if u, err := url.Parse(home); err == nil && u.Host != "" {
		label = html.EscapeString(u.Host)
	}
	return strings.ReplaceAll(page, "<!--HOME-->",
		`<a href="`+esc+`" class="home" title="Back to `+label+`">`+label+` <span aria-hidden="true">&#8599;</span></a>`)
}

var consoleHTML = strings.ReplaceAll(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Abhed Console</title>
<link rel="icon" href="data:image/svg+xml,%3Csvg%20viewBox%3D%220%200%20256%20256%22%20xmlns%3D%22http%3A%2F%2Fwww.w3.org%2F2000%2Fsvg%22%3E%3Cdefs%3E%3ClinearGradient%20id%3D%22fwall%22%20x1%3D%220%22%20y1%3D%220%22%20x2%3D%221%22%20y2%3D%221%22%3E%3Cstop%20offset%3D%220%25%22%20stop-color%3D%22%235CC4FF%22%2F%3E%3Cstop%20offset%3D%2255%25%22%20stop-color%3D%22%232A8CF0%22%2F%3E%3Cstop%20offset%3D%22100%25%22%20stop-color%3D%22%230B3C8C%22%2F%3E%3C%2FlinearGradient%3E%3CradialGradient%20id%3D%22fcore%22%20cx%3D%2240%25%22%20cy%3D%2235%25%22%20r%3D%2270%25%22%3E%3Cstop%20offset%3D%220%25%22%20stop-color%3D%22%23FFFFFF%22%2F%3E%3Cstop%20offset%3D%2270%25%22%20stop-color%3D%22%23DDEFFF%22%2F%3E%3Cstop%20offset%3D%22100%25%22%20stop-color%3D%22%239ED2FF%22%2F%3E%3C%2FradialGradient%3E%3CradialGradient%20id%3D%22fglow%22%20cx%3D%2250%25%22%20cy%3D%2250%25%22%20r%3D%2250%25%22%3E%3Cstop%20offset%3D%220%25%22%20stop-color%3D%22%235CC4FF%22%20stop-opacity%3D%22.55%22%2F%3E%3Cstop%20offset%3D%22100%25%22%20stop-color%3D%22%235CC4FF%22%20stop-opacity%3D%220%22%2F%3E%3C%2FradialGradient%3E%3C%2Fdefs%3E%3Cpath%20d%3D%22M218.6%2090.5%20L165.5%2037.4%20L90.5%2037.4%20L37.4%2090.5%20L37.4%20165.5%20L90.5%20218.6%20L165.5%20218.6%20L218.6%20165.5%20Z%22%20fill%3D%22none%22%20stroke%3D%22url%28%23fwall%29%22%20stroke-width%3D%2224%22%20stroke-linejoin%3D%22round%22%2F%3E%3Ccircle%20cx%3D%22128%22%20cy%3D%22128%22%20r%3D%2262%22%20fill%3D%22none%22%20stroke%3D%22url%28%23fwall%29%22%20stroke-width%3D%226%22%20opacity%3D%22.45%22%2F%3E%3Ccircle%20cx%3D%22128%22%20cy%3D%22128%22%20r%3D%2250%22%20fill%3D%22url%28%23fglow%29%22%2F%3E%3Ccircle%20cx%3D%22128%22%20cy%3D%22128%22%20r%3D%2223%22%20fill%3D%22url%28%23fcore%29%22%2F%3E%3C%2Fsvg%3E">
<style>
:root{
  --bg:#F4F6FA; --surface:#FFFFFF; --raised:#FFFFFF; --sunken:#E6EBF3;
  --line:#DCE3EC; --line-strong:#C4CFDD;
  --ink:#0F141B; --ink-2:#3A4757; --muted:#697786;
  --accent:#0F63C4; --accent-soft:#E2EDFB; --accent-line:#0F63C4;
  --running:#1F6FB8; --done:#1A7F4B; --waiting:#9A6A16; --error:#C0392F;
  --running-bg:#E3EEF8; --done-bg:#E3F3EA; --waiting-bg:#FAF0DC; --error-bg:#FBE9E7;
  --accent-2:#7A3FE0; --danger:#C0392F; --danger-bg:#FBE9E7; --glow:0 0 0 transparent;
  --mono:"JetBrains Mono",ui-monospace,SFMono-Regular,"SF Mono",Menlo,Consolas,"Liberation Mono",monospace;
  --sans:-apple-system,BlinkMacSystemFont,"Inter","Segoe UI",system-ui,Roboto,sans-serif;
  --rail:280px; --drawer:420px;
}
@media (prefers-color-scheme:dark){
  :root:not([data-theme="light"]){
    --bg:#070B12; --surface:#0E1521; --raised:#16202F; --sunken:#0A101A;
    --line:#252D3A; --line-strong:#333D4D;
    --ink:#E8EDF4; --ink-2:#BAC6D4; --muted:#8A96A8;
    --accent:#3BA9FF; --accent-soft:#0B2540; --accent-line:#3BA9FF;
    --running:#4C8FD6; --done:#3FAF6C; --waiting:#D4A03C; --error:#E05A52;
    --running-bg:#132436; --done-bg:#0F2419; --waiting-bg:#241C0C; --error-bg:#2A1412;
    --accent-2:#8B6CFF; --danger:#FF6B6B; --danger-bg:rgba(255,80,80,.14); --glow:0 0 18px rgba(59,169,255,.35);
  }
}
:root[data-theme="dark"]{
  --bg:#070B12; --surface:#0E1521; --raised:#16202F; --sunken:#0A101A;
  --line:#252D3A; --line-strong:#333D4D;
  --ink:#E8EDF4; --ink-2:#BAC6D4; --muted:#8A96A8;
  --accent:#3BA9FF; --accent-soft:#0B2540; --accent-line:#3BA9FF;
  --running:#4C8FD6; --done:#3FAF6C; --waiting:#D4A03C; --error:#E05A52;
  --running-bg:#132436; --done-bg:#0F2419; --waiting-bg:#241C0C; --error-bg:#2A1412;
  --accent-2:#8B6CFF; --danger:#FF6B6B; --danger-bg:rgba(255,80,80,.14); --glow:0 0 18px rgba(59,169,255,.35);
}

*{box-sizing:border-box}
html,body{height:100%}
body{margin:0;background:var(--bg);color:var(--ink);font-family:var(--sans);
  font-size:13.5px;line-height:1.55;-webkit-font-smoothing:antialiased;overflow:hidden}
button,select,textarea,input{font:inherit;color:inherit}
:focus-visible{outline:2px solid var(--accent);outline-offset:1px;border-radius:3px}

/* ---------------------------------------------------------------- chrome */
.top{height:48px;display:flex;align-items:center;gap:14px;padding:0 16px;
  background:var(--surface);border-bottom:1px solid var(--line);flex:none;
  position:relative;z-index:3}
/* A hairline of the accent under the bar: the one place the console glows. */
.top::after{content:"";position:absolute;left:0;right:0;bottom:-1px;height:1px;
  background:linear-gradient(90deg,var(--accent),var(--accent-2) 40%,transparent 80%);opacity:.55}
.brand{display:flex;align-items:center;gap:8px}
/* Both of these belong to the phone layout and are switched on there. Hiding
   them here rather than adding them conditionally in JS keeps one DOM at
   every width, so nothing has to be rebuilt when the screen rotates. */
/* The model picker. Only rendered when more than one provider is configured:
   a dropdown offering a single choice implies an option that is not there. */
#mdlpick{background:var(--sunken);color:var(--ink);border:1px solid var(--line);
  border-radius:6px;font-family:var(--mono);font-size:11.5px;padding:2px 6px;
  max-width:180px;cursor:pointer}
#mdlpick:hover{border-color:var(--accent)}
#mdlpick:disabled{opacity:.55;cursor:not-allowed}
.note-line{font-family:var(--mono);font-size:11.5px;color:var(--muted);
  padding:7px 16px;border-left:2px solid var(--line-strong);margin:8px 0}

.railtoggle{display:none;align-items:center;justify-content:center;
  width:32px;height:32px;flex:none;background:none;border:1px solid var(--line);
  border-radius:7px;color:var(--ink-2);cursor:pointer;padding:0}
.railtoggle:active{background:var(--sunken)}
.scrim{display:none}
@media (max-width:760px){.scrim{display:block}}
.mark{width:24px;height:24px;flex:none;
  filter:drop-shadow(0 0 6px rgba(59,169,255,.35))}
.brand b{font-size:14.5px;font-weight:700;letter-spacing:-.02em}
.brand span{font-family:var(--mono);font-size:10.5px;color:var(--muted)}
.top .spacer{flex:1}
.home{text-decoration:none;color:var(--ink-2);font-family:var(--mono);font-size:11.5px;margin-left:10px;padding:3px 8px;border:1px solid var(--line);border-radius:5px;white-space:nowrap}
.home:hover{color:var(--accent);border-color:var(--accent)}
.stat{font-family:var(--mono);font-size:11px;color:var(--muted);display:flex;
  align-items:center;gap:6px;white-space:nowrap}
.stat b{color:var(--ink-2);font-weight:500}
.led{width:7px;height:7px;border-radius:50%;background:var(--muted);flex:none}
.led.up{background:var(--done);box-shadow:0 0 0 3px var(--done-bg)}
.led.down{background:var(--error);box-shadow:0 0 0 3px var(--error-bg)}
.who-chip{display:inline-flex;align-items:center;gap:6px;padding:2px 8px;
  border-radius:11px;background:var(--sunken);border:1px solid var(--line);
  font-family:var(--mono);font-size:10.5px;color:var(--ink-2)}
.who-chip::before{content:"";width:5px;height:5px;border-radius:50%;
  background:var(--done);flex:none}
.top a.ghost{text-decoration:none;line-height:1.6}

/* ---------------------------------------------------------------- shell */
.shell{display:grid;grid-template-columns:var(--rail) minmax(0,1fr) 0;
  height:calc(100vh - 48px);transition:grid-template-columns .18s ease}
.shell.open{grid-template-columns:var(--rail) minmax(0,1fr) var(--drawer)}
@media (prefers-reduced-motion:reduce){.shell{transition:none}}
.rail{background:var(--surface);border-right:1px solid var(--line);
  display:flex;flex-direction:column;min-height:0}
.stage{display:flex;flex-direction:column;min-height:0;background:var(--bg)}
/* The drawer opens only when there is something to look at: a file the agent
   read or wrote, or output worth reading in full. Closed by default, so the
   conversation gets the width it deserves. */
.drawer{background:var(--surface);border-left:1px solid var(--line);
  overflow:hidden;min-height:0;display:flex;flex-direction:column}
.shell:not(.open) .drawer{border-left:0}
.drawer-head{height:38px;display:flex;align-items:center;gap:8px;padding:0 10px 0 15px;
  border-bottom:1px solid var(--line);flex:none;font-family:var(--mono);font-size:11px}
.drawer-head .name{color:var(--ink);overflow:hidden;text-overflow:ellipsis;
  white-space:nowrap;flex:1}
.drawer-head .kind{color:var(--muted);flex:none}
.drawer-body{flex:1;overflow:auto;min-height:0}
.drawer-body pre{margin:0;padding:14px 16px;font-family:var(--mono);font-size:11.5px;
  line-height:1.6;color:var(--ink-2);white-space:pre;tab-size:4}
.drawer-body .ln{color:var(--muted);user-select:none;display:inline-block;
  width:3.2em;text-align:right;padding-right:1.1em}
.x{background:none;border:0;color:var(--muted);cursor:pointer;font-size:16px;
  line-height:1;padding:2px 6px;border-radius:4px;flex:none}
.x:hover{background:var(--sunken);color:var(--ink)}
.openfile{display:block;margin:0 0 7px;background:var(--surface);
  border:1px solid var(--line);border-radius:5px;padding:3px 9px;
  font-family:var(--mono);font-size:10.5px;color:var(--accent);cursor:pointer}
.openfile:hover{border-color:var(--accent);background:var(--accent-soft)}

/* ------------------------------------------------------------ workbench */
/* The workspace, read-only: a tree or the list of changed files on the left,
   the file or its diff on the right. It borrows the drawer's column and takes
   most of the width, because code needs more room than a tool result does. */
@media (min-width:1181px){
  .shell.open.wide{grid-template-columns:var(--rail) minmax(360px,32%) minmax(0,1fr)}
}
/* Between a phone and a wide screen there is room for the conversation or the
   code, not both: squeezed beside it, the transcript wrapped a path one letter
   to a line. The panel takes the stage's place until it is closed. */
@media (min-width:761px) and (max-width:1180px){
  .shell.open.wide{grid-template-columns:var(--rail) minmax(0,1fr)}
  .shell.open.wide .stage{display:none}
}
.wbtabs{display:flex;gap:4px;flex:none}
.wbtabs[hidden]{display:none}
.wbtabs button[aria-selected="true"]{background:var(--accent-soft);
  border-color:var(--accent);color:var(--accent)}
.wb{display:grid;grid-template-columns:minmax(150px,32%) minmax(0,1fr);height:100%;min-height:0}
.wb-side{overflow:auto;min-height:0;padding:6px 0;border-right:1px solid var(--line)}
.wb-main{display:flex;flex-direction:column;min-width:0;min-height:0}
.wb-view{flex:1;overflow:auto;min-height:0}
.wb-bar{display:flex;align-items:center;gap:8px;min-height:32px;padding:4px 12px;flex:none;
  border-bottom:1px solid var(--line);font-family:var(--mono);font-size:11px;color:var(--muted)}
.wb-bar .nm{flex:1;color:var(--ink);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.wb-back{display:none}
.wb-row{display:flex;align-items:center;gap:6px;width:100%;background:none;border:0;
  text-align:left;padding:3px 10px;font-family:var(--mono);font-size:11.5px;
  color:var(--ink-2);cursor:pointer;white-space:nowrap}
.wb-row:hover{background:var(--sunken)}
.wb-row[aria-current="true"]{background:var(--accent-soft);color:var(--accent)}
.wb-row .tw{width:10px;flex:none;color:var(--muted)}
.wb-row .nm{overflow:hidden;text-overflow:ellipsis}
.wb-row .ct{margin-left:auto;flex:none;font-size:10.5px;color:var(--muted)}
.wb-row .plus{color:var(--done)}
.wb-row .minus{color:var(--error)}
.wb-note{padding:10px 14px;font-family:var(--mono);font-size:11px;color:var(--muted)}
.diff{padding:8px 0;font-family:var(--mono);font-size:11.5px;line-height:1.6;min-width:max-content}
.diff div{padding:0 16px;white-space:pre;tab-size:4;color:var(--ink-2)}
.diff .add{background:var(--done-bg);color:var(--done)}
.diff .del{background:var(--error-bg);color:var(--error)}
.diff .hunk{background:var(--accent-soft);color:var(--accent)}
.diff .meta{color:var(--muted)}
/* Two columns do not fit a phone, so it shows one at a time: the list, then
   the file with a way back. */
@media (max-width:760px){
  .wb{grid-template-columns:minmax(0,1fr)}
  .wb-side{border-right:0}
  .wb-row{padding-top:9px;padding-bottom:9px;font-size:13px}
  .wb .wb-main,.wb.viewing .wb-side{display:none}
  .wb.viewing .wb-main{display:flex}
  .wb-back{display:inline-block}
}

/* ---------------------------------------------------------------- composer */
.composer{padding:11px;border-bottom:1px solid var(--line);flex:none}

/* Attachments. The chips sit above the controls so a queued file is visible
   while you type the question about it. */
.attach{display:flex;align-items:center;justify-content:center;width:28px;height:28px;
  background:var(--sunken);border:1px solid var(--line);border-radius:7px;
  color:var(--muted);cursor:pointer;flex:none;transition:border-color .14s,color .14s}
.attach:hover{border-color:var(--accent);color:var(--accent)}
.files{display:flex;flex-wrap:wrap;gap:6px;padding:0 2px 8px}
.files:empty{display:none}
.chipf{display:inline-flex;align-items:center;gap:6px;max-width:100%;
  background:var(--sunken);border:1px solid var(--line);border-radius:6px;
  padding:3px 8px;font-family:var(--mono);font-size:11px;color:var(--ink-2)}
.chipf.busy{opacity:.6}
.chipf.bad{background:var(--warn-bg);color:var(--warn);border-color:transparent}
.chipf b{font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.chipf .x{cursor:pointer;color:var(--muted);font-size:13px;line-height:1}
.chipf .x:hover{color:var(--warn)}
.new{width:100%;display:flex;align-items:center;justify-content:center;gap:7px;
  background:var(--accent);border:1px solid var(--accent);border-radius:9px;
  padding:9px 12px;font-size:12.5px;font-weight:650;cursor:pointer;color:var(--btn-ink,#04121F);
  box-shadow:var(--glow);transition:filter .14s,transform .14s}
.new:hover{filter:brightness(1.08);transform:translateY(-1px)}
.new span{font-size:15px;line-height:1}
/* Filter the rail. Instant, client-side, on the prompt text the list already
   has: a person with sixty chats needs to find one, not scroll for it. */
.search{width:100%;margin-top:8px;background:var(--sunken);border:1px solid var(--line);border-radius:8px;
  padding:6px 10px;font-size:12px;color:var(--ink)}
.search::placeholder{color:var(--muted)}
.search:focus{outline:none;border-color:var(--accent)}
.grp{font-family:var(--mono);font-size:9.5px;letter-spacing:.1em;text-transform:uppercase;color:var(--muted);
  padding:10px 14px 4px;position:sticky;top:0;background:var(--surface);z-index:1}

/* The dock is the chat input: under the conversation, grows with the text,
   never scrolls away. */
.dock{flex:none;padding:10px 22px 16px;
  background:linear-gradient(to bottom,transparent,var(--bg) 24%)}
.dockwrap{max-width:760px;margin:0 auto;background:var(--surface);
  border:1px solid var(--line);border-radius:16px;padding:12px 14px 10px;
  box-shadow:0 2px 12px -6px rgba(0,0,0,.3)}
.dockwrap:focus-within{border-color:var(--accent);
  box-shadow:0 0 0 3px var(--accent-soft),var(--glow)}
.dockrow{display:flex;align-items:center;gap:9px;margin-top:7px}
.dockhint{flex:1;font-family:var(--mono);font-size:10px;color:var(--muted)}
textarea{width:100%;min-height:22px;max-height:180px;resize:none;background:none;
  border:0;padding:0;font-size:13.5px;line-height:1.6;display:block}
textarea::placeholder{color:var(--muted)}
textarea:focus{outline:none}
.composer .row{display:flex;gap:8px;margin-top:8px;align-items:stretch}
select{background:var(--sunken);border:1px solid var(--line);border-radius:6px;
  padding:0 26px 0 9px;font-size:12px;font-family:var(--mono);cursor:pointer;
  appearance:none;background-image:linear-gradient(45deg,transparent 50%,currentColor 50%),
    linear-gradient(135deg,currentColor 50%,transparent 50%);
  background-position:calc(100% - 14px) 52%,calc(100% - 9px) 52%;
  background-size:5px 5px,5px 5px;background-repeat:no-repeat}
.go{width:32px;height:32px;flex:none;background:var(--accent);border:0;color:var(--btn-ink,#04121F);
  border-radius:50%;font-size:15px;font-weight:700;line-height:1;cursor:pointer;display:grid;
  place-items:center;transition:transform .12s;box-shadow:var(--glow)}
.go:hover:not(:disabled){transform:scale(1.06)}
.go:hover:not(:disabled){filter:brightness(1.08)}
.go:disabled{opacity:.45;cursor:not-allowed}
.hint{font-family:var(--mono);font-size:10px;color:var(--muted);margin-top:7px}

/* ---------------------------------------------------------------- rail */
.rail-head{display:flex;align-items:center;justify-content:space-between;
  padding:10px 14px 6px;flex:none}
.rail-head span{font-family:var(--mono);font-size:10px;letter-spacing:.1em;
  text-transform:uppercase;color:var(--muted)}
.list{flex:1;overflow-y:auto;min-height:0}
.item{width:100%;text-align:left;background:none;border:0;border-bottom:1px solid var(--line);
  padding:10px 14px 10px 12px;cursor:pointer;display:block;border-left:2px solid transparent;
  position:relative;font:inherit;color:inherit}
.item:hover,.item:focus-within{background:var(--sunken)}
.item[aria-current="true"]{background:var(--sunken);border-left-color:var(--accent)}
.item:focus-visible{outline:2px solid var(--accent);outline-offset:-2px}
/* Delete is revealed on hover or keyboard focus so the list stays quiet, and
   it is a real button so it is reachable without a mouse. */
.item .del{position:absolute;top:8px;right:8px;width:24px;height:24px;border:0;border-radius:6px;
  background:transparent;color:var(--muted);cursor:pointer;opacity:0;font:inherit;line-height:1;
  display:grid;place-items:center;transition:opacity .12s,background .12s,color .12s}
.item:hover .del,.item:focus-within .del,.item[aria-current="true"] .del{opacity:1}
.item .del:hover,.item .del:focus-visible{background:var(--danger-bg);color:var(--danger);opacity:1}
.item .del svg{width:13px;height:13px;stroke:currentColor;fill:none;stroke-width:1.8;stroke-linecap:round;stroke-linejoin:round}
.item.confirm .q{color:var(--muted)}
.item .ask{display:flex;align-items:center;gap:8px;font-size:11.5px;color:var(--ink)}
.item .ask b{font-weight:600}
.item .ask button{font:inherit;font-size:11px;padding:3px 9px;border-radius:5px;cursor:pointer;
  border:1px solid var(--line);background:transparent;color:var(--ink)}
.item .ask button.yes{border-color:var(--danger);color:var(--danger)}
.item .ask button.yes:hover{background:var(--danger);color:#fff}
@media (hover:none){.item .del{opacity:1}}
.item .q{font-size:12.5px;line-height:1.45;margin-bottom:5px;color:var(--ink);
  display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.item .m{display:flex;align-items:center;gap:7px;font-family:var(--mono);
  font-size:10px;color:var(--muted)}
.pill{padding:1px 6px;border-radius:3px;font-size:9.5px;letter-spacing:.05em;
  text-transform:uppercase;font-weight:600;white-space:nowrap}
.pill.running{background:var(--running-bg);color:var(--running)}
.pill.completed,.pill.done{background:var(--done-bg);color:var(--done)}
.pill.waiting_approval{background:var(--waiting-bg);color:var(--waiting)}
.pill.error,.pill.max_turns,.pill.policy_denied,.pill.retry_exhausted,
.pill.max_budget,.pill.user_interrupt{background:var(--error-bg);color:var(--error)}

/* ---------------------------------------------------------------- stage */
.stage-head{height:38px;display:flex;align-items:center;gap:10px;padding:0 18px;
  border-bottom:1px solid var(--line);background:var(--surface);flex:none;
  font-family:var(--mono);font-size:11px;color:var(--muted)}
.stage-head .id{color:var(--ink-2);min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.stage-head .ghost{flex:none}
.stage-head .spacer{flex:1}
.ghost{background:none;border:1px solid var(--line);border-radius:5px;
  padding:3px 9px;font-family:var(--mono);font-size:10.5px;color:var(--ink-2);cursor:pointer}
.ghost:hover{background:var(--sunken);border-color:var(--line-strong)}
.transcript{flex:1;overflow-y:auto;padding:22px 22px 8px;min-height:0}
.transcript > *{max-width:760px;margin-left:auto;margin-right:auto}
.empty{display:flex;flex-direction:column;align-items:center;justify-content:center;
  min-height:100%;gap:8px;color:var(--muted);text-align:center;padding:12px 0}
.empty .k{font-family:var(--sans);font-size:24px;font-weight:750;letter-spacing:-.035em;color:var(--ink);line-height:1.15}
.empty .k .hl{background:linear-gradient(92deg,var(--accent),var(--accent-2));-webkit-background-clip:text;background-clip:text;color:transparent}
.empty .s{font-size:13.5px;max-width:44ch;line-height:1.6}
.empty .ex{display:grid;grid-template-columns:1fr;gap:8px;margin-top:18px;width:100%;
  max-width:330px}
/* Six examples in one column push the mark and the word Ready off the top of
   the pane on a laptop. Two columns keep the block inside the viewport where
   there is room; the column view is kept for phones, where a wide grid of
   cards would be two half-width cards you cannot read. */
@media (min-width:700px){.empty .ex{grid-template-columns:1fr 1fr;max-width:600px;gap:8px}}
.mark-lg{width:56px;height:56px;margin-bottom:6px;filter:drop-shadow(0 0 18px rgba(59,169,255,.45))}
.chip{background:var(--surface);border:1px solid var(--line);border-radius:12px;
  padding:11px 13px;font-size:12.5px;color:var(--ink-2);cursor:pointer;text-align:left;line-height:1.5;
  transition:border-color .14s,transform .14s,box-shadow .14s}
.chip:hover{border-color:var(--accent);transform:translateY(-2px);color:var(--ink);
  box-shadow:0 14px 30px -18px rgba(59,169,255,.6)}
.chip .lbl{font-family:var(--mono);font-size:9.5px;letter-spacing:.08em;text-transform:uppercase;color:var(--accent);margin-bottom:4px;font-weight:600}

/* turn grouping: a vertical spine ties a turn's calls together */
.turn{position:relative;padding-left:20px;margin-bottom:4px}
.turn::before{content:"";position:absolute;left:5px;top:14px;bottom:2px;
  width:1px;background:var(--line)}
.turn:last-child::before{display:none}

.said{background:var(--raised);border:1px solid var(--line);border-radius:9px;
  padding:12px 15px;margin:0 0 12px;white-space:pre-wrap;line-height:1.65;
  overflow-wrap:anywhere;box-shadow:0 1px 2px rgba(0,0,0,.05);
  animation:rise .22s cubic-bezier(.2,.7,.3,1) both}
@keyframes rise{from{opacity:0;transform:translateY(4px)}to{opacity:1;transform:none}}
@media (prefers-reduced-motion:reduce){.said{animation:none}}
.said.user{background:linear-gradient(135deg,var(--accent-soft),var(--raised));border-color:color-mix(in srgb,var(--accent) 45%,transparent)}
.said{border-radius:12px}
.said .who{font-family:var(--mono);font-size:9.5px;letter-spacing:.1em;
  text-transform:uppercase;color:var(--muted);margin-bottom:6px}

.call{margin-bottom:10px}
.call .hdr{display:flex;align-items:baseline;gap:8px;font-family:var(--mono);
  font-size:11.5px;position:relative}
/* A finished call collapses to its header. The whole header is the toggle, so
   the click target is the line you are already reading. */
.call .hdr[role=button]{cursor:pointer;user-select:none}
.call .hdr[role=button]:hover .tool{text-decoration:underline}
.call .caret{display:inline-block;width:9px;flex:none;color:var(--muted);
  transition:transform .12s ease}
.call.collapsed .caret{transform:rotate(-90deg)}
.call.collapsed .out,.call.collapsed .openfile,.call.collapsed .tag{display:none}
.call.collapsed .hdr .peek{display:inline}
.call .hdr .peek{display:none;color:var(--muted);font-size:10.5px;
  overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:34ch}

/* Rendered markdown in a reply. Spacing is tight on purpose: a chat bubble is
   not an article, and the default margins leave a wall of gaps. */
.md > :first-child{margin-top:0}
.md > :last-child{margin-bottom:0}
.md p{margin:.5em 0}
.md h3,.md h4,.md h5,.md h6{margin:1em 0 .4em;font-size:14px;font-weight:600}
.md ul,.md ol{margin:.5em 0;padding-left:1.4em}
.md li{margin:.2em 0}
.md code{font-family:var(--mono);font-size:12px;background:var(--sunken);
  padding:1px 4px;border-radius:3px}
.md pre.code{font-family:var(--mono);font-size:11.5px;line-height:1.55;
  background:var(--sunken);border-left:2px solid var(--line-strong);
  border-radius:0 5px 5px 0;padding:.6rem .8rem;margin:.6em 0;
  white-space:pre-wrap;overflow-x:auto}
.md table{border-collapse:collapse;margin:.6em 0;font-size:13px;display:block;
  overflow-x:auto;max-width:100%}
.md th,.md td{border:1px solid var(--line);padding:.3em .6em;text-align:left}
.md th{background:var(--sunken);font-weight:600}
.md a{color:var(--accent)}
.md hr{border:0;border-top:1px solid var(--line);margin:.8em 0}

/* Model reasoning, minimised by default: reference material, not the reply. */
.think{margin:6px 0 10px}
.think .hdr{display:flex;align-items:center;gap:7px;font-family:var(--mono);
  font-size:11px;color:var(--muted);cursor:pointer;user-select:none;padding:2px 0}
.think .hdr:hover{color:var(--ink-2)}
.think .caret{display:inline-block;width:9px;flex:none;transition:transform .12s ease}
.think.collapsed .caret{transform:rotate(-90deg)}
.think.collapsed .body{display:none}
.think .body{font-family:var(--mono);font-size:11px;line-height:1.6;
  color:var(--ink-2);background:var(--sunken);border-left:2px solid var(--line-strong);
  border-radius:0 5px 5px 0;padding:8px 11px;margin-top:5px;white-space:pre-wrap;
  max-height:340px;overflow-y:auto}
.call .hdr::before{content:"";position:absolute;left:-18px;top:6px;width:7px;height:7px;
  border-radius:50%;background:var(--accent);box-shadow:0 0 0 3px var(--bg)}
.call.err .hdr::before{background:var(--error)}
.call .tool{color:var(--accent);font-weight:600}
.call.err .tool{color:var(--error)}
.call .arg{color:var(--muted);overflow-wrap:anywhere}
.call .ms{margin-left:auto;color:var(--muted);font-size:10px;
  font-variant-numeric:tabular-nums;flex:none}
.out{font-family:var(--mono);font-size:11px;line-height:1.55;color:var(--ink-2);
  background:var(--sunken);border-left:2px solid var(--line-strong);
  border-radius:0 5px 5px 0;padding:8px 11px;margin-top:6px;white-space:pre-wrap;
  overflow-x:auto;max-height:300px;overflow-y:auto}
.out.err{border-left-color:var(--error);color:var(--error)}
.out.untrusted{border-left-color:var(--waiting)}
.tag{display:inline-block;font-family:var(--mono);font-size:9px;letter-spacing:.06em;
  text-transform:uppercase;padding:1px 5px;border-radius:3px;margin-bottom:5px;
  background:var(--waiting-bg);color:var(--waiting)}

.approve{border:1px solid var(--waiting);background:var(--waiting-bg);
  border-radius:8px;padding:13px 15px;margin:12px 0}
.approve h4{margin:0 0 4px;font-family:var(--mono);font-size:11.5px;color:var(--waiting)}
.approve p{margin:0 0 9px;font-size:12px;color:var(--ink-2)}
.approve pre{font-family:var(--mono);font-size:11px;background:var(--sunken);
  border-radius:5px;padding:9px;overflow-x:auto;margin:0 0 10px;color:var(--ink-2)}
.approve .row{display:flex;gap:8px}
.approve button{border-radius:5px;padding:5px 13px;font-size:12px;
  font-weight:600;cursor:pointer;border:1px solid var(--line)}
.approve .yes{background:var(--accent);border-color:var(--accent);color:var(--btn-ink,#04121F);box-shadow:var(--glow)}
.approve .no{background:var(--surface)}
.approve .always{background:var(--surface)}

/* The decision, pinned to the tool row it belongs to and kept visible when the
   row collapses — so "approved" is never a gray line floating away from the
   command it approved. */
.call .verdict{margin-left:auto;font-family:var(--mono);font-size:10px;flex:none;
  border-radius:10px;padding:0 8px;border:1px solid var(--waiting);color:var(--waiting)}
.call .verdict.ok{border-color:var(--done);color:var(--done)}
.call .verdict.no{border-color:var(--error);color:var(--error)}
.call .verdict ~ .ms{margin-left:8px}

.note{font-family:var(--mono);font-size:10.5px;color:var(--muted);
  border-top:1px dashed var(--line);padding-top:9px;margin:14px 0 4px;
  display:flex;gap:14px;flex-wrap:wrap}
.thinking{display:flex;align-items:center;gap:9px;font-family:var(--mono);
  font-size:11.5px;color:var(--muted);padding:9px 0 2px}
.thinking .bars{display:flex;gap:2.5px;align-items:flex-end;height:12px}
.thinking .bars i{width:2.5px;height:4px;background:var(--accent);border-radius:1px;
  animation:pulse 1.05s ease-in-out infinite}
.thinking .bars i:nth-child(2){animation-delay:.13s}
.thinking .bars i:nth-child(3){animation-delay:.26s}
.thinking .bars i:nth-child(4){animation-delay:.39s}
@keyframes pulse{0%,100%{height:4px;opacity:.45}50%{height:12px;opacity:1}}
@media (prefers-reduced-motion:reduce){.thinking .bars i{animation:none;height:8px}}
.note b{color:var(--ink-2);font-weight:500;font-variant-numeric:tabular-nums}

/* ---------------------------------------------------------------- inspector */
.insp-sec{padding:13px 15px;border-bottom:1px solid var(--line)}
.insp-sec h3{margin:0 0 9px;font-family:var(--mono);font-size:9.5px;letter-spacing:.11em;
  text-transform:uppercase;color:var(--muted);font-weight:600}
.kv{display:flex;justify-content:space-between;align-items:baseline;gap:10px;
  font-family:var(--mono);font-size:11.5px;padding:3px 0}
.kv dt{color:var(--muted)}
.kv dd{margin:0;color:var(--ink);font-variant-numeric:tabular-nums;text-align:right;
  overflow-wrap:anywhere}
.meter{height:4px;background:var(--sunken);border-radius:2px;overflow:hidden;margin-top:7px}
.meter i{display:block;height:100%;background:var(--accent);border-radius:2px}
.tally{display:flex;justify-content:space-between;font-family:var(--mono);
  font-size:11.5px;padding:3px 0}
.tally span:first-child{color:var(--accent)}
.tally span:last-child{color:var(--ink);font-variant-numeric:tabular-nums}
.insp-empty{padding:22px 15px;font-size:12px;color:var(--muted);line-height:1.6}

@media (max-width:1180px){
  .shell{grid-template-columns:var(--rail) minmax(0,1fr)}
  .inspector{display:none}
}

/* ------------------------------------------------------------------ phone */
/* A 272px session rail on a 390px screen leaves about 110px for the
   conversation, which is why this was unusable rather than merely cramped. On
   a phone the rail stops being a column and becomes a slide-over panel: the
   transcript gets the whole width, and the rail is one tap away.

   100vh is also wrong here. Mobile browsers measure it against the viewport
   WITHOUT their own chrome, so the composer sits below the fold and the page
   scrolls when it should not. 100dvh tracks the visible area as the toolbar
   hides and shows; the 100vh line stays first as the fallback for browsers
   that do not know dvh. */
@media (max-width:760px){
  .shell{
    grid-template-columns:minmax(0,1fr);
    height:calc(100vh - 48px);
    height:calc(100dvh - 48px);
  }
  .shell.open{grid-template-columns:minmax(0,1fr)}

  .rail{
    position:fixed;top:48px;left:0;bottom:0;width:min(84vw,300px);
    z-index:20;transform:translateX(-101%);
    transition:transform .2s ease;
    box-shadow:2px 0 22px -8px rgba(0,0,0,.45);
    border-right:1px solid var(--line-strong);
  }
  body.rail-open .rail{transform:translateX(0)}

  /* Tapping the backdrop closes the rail — the gesture people expect, and it
     saves a second trip to the toggle. */
  .scrim{
    position:fixed;inset:48px 0 0;background:rgba(0,0,0,.42);
    opacity:0;pointer-events:none;transition:opacity .2s ease;z-index:19;
  }
  body.rail-open .scrim{opacity:1;pointer-events:auto}

  .railtoggle{display:inline-flex}

  /* The header carries six items that do not fit. Identity and the health
     LED earn their place; model and session counts are detail a phone can
     do without, and they are still on the landing page. */
  .top{gap:9px;padding:0 11px}
  .top .stat{display:none}
  .top .stat#whobox{display:flex;gap:7px}
  .top .who-chip{max-width:104px;overflow:hidden;text-overflow:ellipsis;
    white-space:nowrap}
  #switchuser{display:none}
  .brand .sub{display:none}
  /* The way home stays, as the arrow alone: the word does not fit. */
  .home{font-size:0;padding:5px 8px}
  .home span{font-size:12px}
  .dockhint{display:none}

  /* A 16px font on the input is what stops iOS zooming the whole page when
     the keyboard opens — the single most disorienting thing a mobile web app
     can do. */
  .ask{font-size:16px}

  .drawer{
    position:fixed;inset:48px 0 0;width:100%;z-index:18;
    border-left:0;transform:translateY(101%);
    transition:transform .2s ease;
  }
  .shell.open .drawer{transform:translateY(0)}

  .msg{padding-left:13px;padding-right:13px}
}

/* Landscape phones and small tablets keep the rail but narrow it, rather than
   spending a third of the width on a list of chat titles. */
@media (min-width:761px) and (max-width:1180px){
  :root{--rail:212px}
}
@media (prefers-reduced-motion:reduce){*{transition:none!important;animation:none!important}}
</style>
</head>
<body>

<div class="top">
  <!-- Shown only on a phone, where the rail is a slide-over rather than a
       column. aria-expanded is kept in sync so a screen reader is told what
       the button did, not just that it exists. -->
  <button class="railtoggle" id="railtoggle" type="button"
          aria-label="Show chats" aria-expanded="false" aria-controls="list">
    <svg viewBox="0 0 20 20" aria-hidden="true" width="17" height="17">
      <path d="M3 5h14M3 10h14M3 15h14" stroke="currentColor"
            stroke-width="1.8" stroke-linecap="round" fill="none"/>
    </svg>
  </button>
  <div class="brand">
    <svg class="mark" viewBox="0 0 256 256" aria-hidden="true"><defs><linearGradient id="tt-wall" x1="0" y1="0" x2="1" y2="1"><stop offset="0%" stop-color="#5CC4FF"/><stop offset="55%" stop-color="#2A8CF0"/><stop offset="100%" stop-color="#0B3C8C"/></linearGradient><radialGradient id="tt-core" cx="40%" cy="35%" r="70%"><stop offset="0%" stop-color="#FFFFFF"/><stop offset="70%" stop-color="#DDEFFF"/><stop offset="100%" stop-color="#9ED2FF"/></radialGradient><radialGradient id="tt-glow" cx="50%" cy="50%" r="50%"><stop offset="0%" stop-color="#5CC4FF" stop-opacity=".55"/><stop offset="100%" stop-color="#5CC4FF" stop-opacity="0"/></radialGradient></defs><path d="M218.6 90.5 L165.5 37.4 L90.5 37.4 L37.4 90.5 L37.4 165.5 L90.5 218.6 L165.5 218.6 L218.6 165.5 Z" fill="none" stroke="url(#tt-wall)" stroke-width="24" stroke-linejoin="round"/><circle cx="128" cy="128" r="62" fill="none" stroke="url(#tt-wall)" stroke-width="6" opacity=".45"/><circle cx="128" cy="128" r="50" fill="url(#tt-glow)"/><circle cx="128" cy="128" r="23" fill="url(#tt-core)"/></svg>
    <a href="/" style="text-decoration:none;color:inherit;display:flex;
       align-items:baseline;gap:8px" title="Overview"><b>Abhed</b><span
       id="ver">console</span></a><!--HOME-->
  </div>
  <div class="stat"><span class="led" id="led"></span><span id="health">connecting</span></div>
  <div class="spacer"></div>
  <div class="stat">model
    <b id="mdl">—</b>
    <select id="mdlpick" hidden title="Run this session on a different model"></select>
  </div>
  <div class="stat">active <b id="active">0</b></div>
  <div class="stat" id="whobox" hidden>
    <span class="who-chip" id="who"></span>
    <a class="ghost" href="/ide" title="Editor, terminal and agent side by side">Workbench</a>
    <a class="ghost" id="adminlink" href="/admin" hidden
       title="Who has access, and who no longer does">Admin</a>
    <a class="ghost" id="pwlink" href="/account" hidden title="Change your password">Password</a>
    <a class="ghost" id="switchuser" href="/logout" hidden
       title="Sign in as a different user">Switch</a>
    <a class="ghost" id="signout" href="/logout" hidden>Sign out</a>
  </div>
</div>

<div class="shell">
  <!-- Backdrop behind the slide-over rail. Present at every width but only
       visible on a phone, so no second DOM tree has to be kept in step. -->
  <div class="scrim" id="scrim" hidden></div>

  <!-- session rail -->
  <aside class="rail">
    <div class="composer">
      <button class="new" id="new" type="button"><span>+</span> New chat</button>
      <input class="search" id="search" type="search" placeholder="Search chats" aria-label="Search chats" autocomplete="off">
    </div>
    <div class="rail-head"><span>Chats</span><span id="count"></span></div>
    <div class="list" id="list"></div>
  </aside>

  <!-- transcript -->
  <main class="stage">
    <div class="stage-head">
      <span class="id" id="sid">new chat</span>
      <span class="spacer"></span>
      <button class="ghost" id="wbfiles" type="button" hidden>Files</button>
      <button class="ghost" id="wbchanges" type="button" hidden>Changes</button>
      <button class="ghost" id="stop" hidden>Interrupt</button>
    </div>
    <div class="transcript" id="tx">
      <div class="empty">
        <svg class="mark-lg" viewBox="0 0 256 256" aria-hidden="true"><defs><linearGradient id="tt-wall" x1="0" y1="0" x2="1" y2="1"><stop offset="0%" stop-color="#5CC4FF"/><stop offset="55%" stop-color="#2A8CF0"/><stop offset="100%" stop-color="#0B3C8C"/></linearGradient><radialGradient id="tt-core" cx="40%" cy="35%" r="70%"><stop offset="0%" stop-color="#FFFFFF"/><stop offset="70%" stop-color="#DDEFFF"/><stop offset="100%" stop-color="#9ED2FF"/></radialGradient><radialGradient id="tt-glow" cx="50%" cy="50%" r="50%"><stop offset="0%" stop-color="#5CC4FF" stop-opacity=".55"/><stop offset="100%" stop-color="#5CC4FF" stop-opacity="0"/></radialGradient></defs><path d="M218.6 90.5 L165.5 37.4 L90.5 37.4 L37.4 90.5 L37.4 165.5 L90.5 218.6 L165.5 218.6 L218.6 165.5 Z" fill="none" stroke="url(#tt-wall)" stroke-width="24" stroke-linejoin="round"/><circle cx="128" cy="128" r="62" fill="none" stroke="url(#tt-wall)" stroke-width="6" opacity=".45"/><circle cx="128" cy="128" r="50" fill="url(#tt-glow)"/><circle cx="128" cy="128" r="23" fill="url(#tt-core)"/></svg>
        <div class="k" id="greet">What should we <span class="hl">work on</span>?</div>
        <div class="s">Ask a question, describe a change, or attach a document. Every
          step the agent takes is recorded; pick any chat on the left to replay it.</div>
        <div class="ex" id="examples"></div>
      </div>
    </div>
    <div class="dock">
      <div class="dockwrap">
        <textarea id="q" rows="1" placeholder="Ask anything, or describe a change…"></textarea>
        <div class="files" id="files"></div>
        <div class="dockrow">
          <input type="file" id="file" multiple hidden>
          <button class="attach" id="attach" type="button" title="Attach a document">
            <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
              <path fill="none" stroke="currentColor" stroke-width="1.5"
                stroke-linecap="round"
                d="M10.5 5.5 6 10a1.8 1.8 0 0 0 2.5 2.5l4.5-4.5a3.4 3.4 0 0 0-4.8-4.8L3.4 8.5a5 5 0 0 0 7 7l3.6-3.6"/>
            </svg>
          </button>
          <select id="mode" title="Permission mode">
            <option value="default">default</option>
            <option value="plan">plan</option>
            <option value="accept-edits">accept-edits</option>
            <option value="auto">auto</option>
          </select>
          <span class="dockhint">Enter sends · Shift+Enter for a new line</span>
          <button class="go" id="go" type="button" title="Send">↑</button>
        </div>
      </div>
    </div>
  </main>

  <aside class="drawer" id="drawer" aria-hidden="true">
    <div class="drawer-head">
      <span class="name" id="dname"></span>
      <span class="wbtabs" id="wbtabs" role="tablist" hidden>
        <button class="ghost" id="tabfiles" type="button" role="tab">Files</button>
        <button class="ghost" id="tabchanges" type="button" role="tab">Changes</button>
        <button class="ghost" id="wbreload" type="button" title="Reload">&#8635;</button>
      </span>
      <span class="kind" id="dkind"></span>
      <button class="x" id="dclose" type="button" title="Close" aria-label="Close">×</button>
    </div>
    <div class="drawer-body" id="dbody"></div>
  </aside>
</div>

<script>
"use strict";
const $ = id => document.getElementById(id);

let current = null;      // session id being viewed
let streamEl = null;     // bubble currently receiving streamed text
let streamBody = null;   // its text node
let es = null;           // EventSource
let lastSeq = 0;         // highest seq rendered, for reconnect de-duplication
let live = false;        // is the viewed session still running
// Approval cards awaiting a verdict, by call_id. A replayed session resolves
// them from its own action.approved / action.denied events; anything still
// here when the session ends was never answered.
let approvals = new Map();
let turnEl = null;       // current turn container
const calls = new Map(); // call_id -> DOM node, to attach observations
const stats = {turns:0, tin:0, tout:0, cached:0, tools:{}, reason:null, compactions:0, ctx:0, ctxWindow:0};

/* ------------------------------------------------------------------ api */
async function api(path, opts){
  const r = await fetch(path, {headers:{'Content-Type':'application/json'}, ...opts});
  if(!r.ok){
    let msg = r.statusText;
    try { msg = (await r.json()).error || msg; } catch {}
    throw new Error(msg);
  }
  return r.status === 204 ? null : r.json();
}

/* ------------------------------------------------------------------ chrome */
async function health(){
  try{
    const h = await api('/v1/health');
    $('led').className = 'led up';
    $('health').textContent = 'connected';
    if($('mdlpick').hidden) $('mdl').textContent = h.model;
    $('active').textContent = h.sessions;
  }catch{
    $('led').className = 'led down';
    $('health').textContent = 'unreachable';
  }
}

/* The model picker.
 *
 * Loaded once: the configured provider set does not change while the page is
 * open, and re-fetching it on every health poll would reset the dropdown under
 * anyone who had just changed it.
 *
 * Only shown when there is a real choice. A select with one option tells the
 * user they can pick something when they cannot. */
let providers = [];
async function loadProviders(){
  try{ providers = await api('/v1/providers'); }catch{ return; }
  if(!Array.isArray(providers) || providers.length < 2) return;

  const sel = $('mdlpick');
  sel.textContent = '';
  for(const p of providers){
    const o = document.createElement('option');
    o.value = p.name;
    o.textContent = p.model || p.name;
    o.selected = p.default;
    sel.appendChild(o);
  }
  $('mdl').hidden = true;
  sel.hidden = false;

  sel.onchange = async () => {
    // With no session yet the choice simply applies to the next one, so there
    // is nothing to send — createSession carries the provider.
    if(!current){ return; }
    sel.disabled = true;
    try{
      await api('/v1/sessions/' + current + '/model', {
        method:'POST', body: JSON.stringify({provider: sel.value}),
      });
      note('Model switched to ' + sel.value + ' for this chat.');
    }catch(e){
      // The server refuses a swap mid-turn, which is the common case here.
      note(String(e.message || e));
      const cur = providers.find(p => p.default);
      if(cur) sel.value = cur.name;
    }finally{
      sel.disabled = false;
    }
  };
}

// A one-line status message in the transcript, for things that are neither an
// agent event nor an error worth a dialog.
function note(text){
  const t = $('tx');
  if(!t) return;
  const el = document.createElement('div');
  el.className = 'note-line';
  el.textContent = text;
  t.appendChild(el);
  t.scrollTop = t.scrollHeight;
}

async function refresh(){
  try{
    const list = await api('/v1/sessions');
    list.sort((a,b) => new Date(b.created) - new Date(a.created));
    $('count').textContent = list.length;

    const el = $('list');
    const q = ($('search') && $('search').value || '').trim().toLowerCase();
    // Rebuild only when something changed. The poll runs every few seconds,
    // and rebuilding the rail on every tick tore down whatever the person
    // was doing in it — a hover, a focused row, an open delete confirmation.
    const sig = q + '|' + current + '|' + list.map(s => s.id + ':' + s.state + ':' + (s.prompt || '')).join('\n');
    if(el.dataset.sig === sig) return;
    if(el.querySelector('.item.confirm')) return;   // never yank a question mid-answer
    el.dataset.sig = sig;
    el.textContent = '';
    let grp = null;
    for(const s of list){
      if(q && !(s.prompt || '').toLowerCase().includes(q)) continue;
      const g = dayGroup(s.created);
      if(g !== grp){
        grp = g;
        const h = document.createElement('div');
        h.className = 'grp';
        h.textContent = g;
        el.appendChild(h);
      }
      el.appendChild(sessionRow(s));
    }
  }catch{}
}

function dayGroup(iso){
  const d = new Date(iso), now = new Date();
  const day = 86400000;
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  if(d.getTime() >= start) return 'Today';
  if(d.getTime() >= start - day) return 'Yesterday';
  if(d.getTime() >= start - 6 * day) return 'This week';
  return 'Earlier';
}
if($('search')) $('search').addEventListener('input', () => refresh());

// sessionRow builds one entry in the rail. It is a div acting as a button
// rather than a <button>, because the delete control inside it is itself a
// button and buttons cannot nest.
function sessionRow(s){
  const row = document.createElement('div');
  row.className = 'item';
  row.setAttribute('role','button');
  row.tabIndex = 0;
  row.dataset.id = s.id;
  if(s.id === current) row.setAttribute('aria-current','true');

  const q = document.createElement('div');
  q.className = 'q';
  q.textContent = s.prompt || '(no prompt recorded)';

  const m = document.createElement('div');
  m.className = 'm';
  const pill = document.createElement('span');
  pill.className = 'pill ' + s.state;
  pill.textContent = s.state.replace(/_/g,' ');
  const when = document.createElement('span');
  when.textContent = ago(s.created);
  m.append(pill, when);

  const del = document.createElement('button');
  del.type = 'button';
  del.className = 'del';
  del.title = 'Delete this chat';
  del.setAttribute('aria-label', 'Delete this chat');
  del.innerHTML = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3"/></svg>';
  del.onclick = (e) => { e.stopPropagation(); confirmDelete(row, s, m); };

  row.append(q, m, del);
  const open = () => openSession(s.id, s.state);
  row.onclick = (e) => { if(!row.classList.contains('confirm')) open(); };
  row.onkeydown = (e) => {
    if(e.target !== row) return;
    if(e.key === 'Enter' || e.key === ' '){ e.preventDefault(); open(); }
    if(e.key === 'Delete' || e.key === 'Backspace'){ e.preventDefault(); confirmDelete(row, s, m); }
  };
  return row;
}

// confirmDelete swaps the row's meta line for an inline yes/no. Inline rather
// than a browser dialog: the question stays next to the thing it is about,
// and nothing blocks the rest of the page.
function confirmDelete(row, s, meta){
  if(row.classList.contains('confirm')) return;
  row.classList.add('confirm');
  const ask = document.createElement('div');
  ask.className = 'ask';
  const label = document.createElement('span');
  label.innerHTML = '<b>Delete this chat?</b>';
  const yes = document.createElement('button'); yes.type='button'; yes.className='yes'; yes.textContent='Delete';
  const no  = document.createElement('button'); no.type='button';  no.textContent='Keep';
  const restore = () => { row.classList.remove('confirm'); ask.replaceWith(meta); row.focus(); $('list').dataset.sig = ''; refresh(); };
  no.onclick = (e) => { e.stopPropagation(); restore(); };
  yes.onclick = async (e) => {
    e.stopPropagation();
    yes.disabled = true; yes.textContent = 'Deleting…';
    if(await deleteSession(s.id, restore)){
      // The row goes first: refresh() leaves the rail alone while a
      // confirmation is open, and this one is answered.
      row.remove();
      $('list').dataset.sig = '';
      refresh();
    }
  };
  ask.onkeydown = (e) => { if(e.key === 'Escape'){ e.stopPropagation(); restore(); } };
  ask.append(label, yes, no);
  meta.replaceWith(ask);
  yes.focus();
}

// deleteSession asks the server to remove a chat. A 501 means this
// deployment's store is append-only and deletion was never on offer — said
// plainly, not swallowed. If the open chat is the one deleted, the transcript
// pane is cleared so the page does not keep showing what no longer exists.
async function deleteSession(id, onFail){
  try{
    const r = await fetch('/v1/sessions/' + id, {method:'DELETE', credentials:'same-origin'});
    if(r.status === 501){
      note('This deployment keeps an append-only record; chats cannot be deleted here.');
      onFail && onFail();
      return false;
    }
    if(!r.ok && r.status !== 204){
      note('Could not delete this chat (' + r.status + ').');
      onFail && onFail();
      return false;
    }
    if(current === id) newChat();
    return true;
  }catch(err){
    note('Could not delete this chat: ' + (err && err.message || err));
    onFail && onFail();
    return false;
  }
}

function ago(iso){
  const s = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  if(s < 60) return Math.floor(s) + 's ago';
  if(s < 3600) return Math.floor(s/60) + 'm ago';
  if(s < 86400) return Math.floor(s/3600) + 'h ago';
  return Math.floor(s/86400) + 'd ago';
}

/* ------------------------------------------------------------------ session */
// openSession switches the view to a session, live or historical.
//
// state comes from the session list (running | waiting_approval | done). It
// matters because a finished session replays its whole backlog, including the
// original approval request: assuming every opened session is live rebuilt
// those as clickable prompts for decisions already made.
function openSession(id, state){
  // On a phone the rail covers the transcript, so opening a chat has to
  // dismiss it — otherwise the user taps a chat and still sees the list.
  setRail(false);
  if(es){ es.close(); es = null; }
  current = id; lastSeq = 0; live = (state !== 'done'); turnEl = null;
  streamEl = null; streamBody = null;
  calls.clear();
  approvals.clear();
  Object.assign(stats, {turns:0, tin:0, tout:0, cached:0, tools:{}, reason:null, compactions:0, ctx:0, ctxWindow:0});

  $('tx').textContent = '';
  $('sid').textContent = id;
  $('stop').hidden = false;
  $('wbfiles').hidden = $('wbchanges').hidden = false;
  // The panel shows one session's workspace and changes, so it follows the
  // session rather than going on showing the last one's.
  if(wb.open) openWorkbench(wb.tab);
  refresh();
  connect(id);
}

// connect opens the event stream and keeps it open.
//
// EventSource fires onerror on ANY interruption, including the normal close at
// the end of a stream. Reconnecting is what makes a long session survive a
// dropped connection; de-duplicating by seq is what stops the replayed backlog
// from rendering twice.
function connect(id){
  es = new EventSource('/v1/sessions/' + id + '/events');

  es.onmessage = e => {
    let ev; try{ ev = JSON.parse(e.data); }catch{ return; }
    if(ev.seq <= lastSeq) return;
    lastSeq = ev.seq;
    render(ev);
    workbenchSaw(ev);
    };

  es.onerror = () => {
    if(!es) return;
    es.close(); es = null;
    // A finished session's stream closes normally once the backlog is sent.
    // Only a live session is worth reconnecting to.
    if(live && current === id){
      setTimeout(() => { if(current === id && !es) connect(id); }, 1500);
    }
  };
}

/* ------------------------------------------------------------------ render */
function node(cls, text){
  const d = document.createElement('div');
  if(cls) d.className = cls;
  if(text !== undefined) d.textContent = text;
  return d;
}

function newTurn(){
  turnEl = node('turn');
  $('tx').appendChild(turnEl);
  return turnEl;
}

function render(ev){
  const tx = $('tx');
  const p = ev.payload || {};

  switch(ev.type){
    case 'user.message': {
      const b = node('said user');
      b.append(node('who','you'), document.createTextNode(p.text || ''));
      tx.appendChild(b);
      newTurn();
      stats.turns++;
      break;
    }

    case 'agent.delta': {
      // Stream into a bubble created on the first fragment. agent.message
      // arrives afterwards with the complete text and closes the bubble
      // rather than appending a second copy.
      hideThinking();
      if(!streamEl){
        streamEl = node('said');
        streamEl.append(node('who','abhed'));
        streamBody = document.createTextNode('');
        streamEl.appendChild(streamBody);
        (turnEl || tx).appendChild(streamEl);
      }
      streamBody.appendData(p.text || '');
      break;
    }

    case 'agent.message': {
      hideThinking();
      if(streamEl){
        // The deltas already rendered this. Reconcile against the
        // authoritative text in case a fragment was dropped on reconnect,
        // then close the bubble.
        if((p.text || '') !== streamBody.data) streamBody.data = p.text || '';
        // The deltas streamed plain text; render it now that it is whole.
        const holder = document.createElement('div');
        holder.className = 'md';
        holder.innerHTML = md(streamBody.data);
        streamBody.replaceWith(holder);
        streamEl = null; streamBody = null;
        break;
      }
      if(!(p.text || '').trim()) break;
      // Belt and braces: agent.message repeats text the deltas already
      // streamed, so anything that loses the stream reference mid-turn - a
      // handler that clears it, a replay that interleaves events differently -
      // would otherwise print the whole reply twice. Adopt the bubble the
      // deltas built rather than trusting a variable to still be set.
      const streamed = lastStreamedBubble();
      if(streamed && streamed.body.data.trim() === (p.text || '').trim()){
        // Same text: replace the streamed plain draft with the rendered form.
        // Markdown cannot be applied to a fragment, so the deltas stream raw
        // and the finished answer is formatted here.
        const holder = document.createElement('div');
        holder.className = 'md';
        holder.innerHTML = md(p.text);
        streamed.body.replaceWith(holder);
        break;
      }
      if(streamed && (p.text || '').startsWith(streamed.body.data.trim().slice(0, 200))
         && streamed.body.data.trim() !== ''){
        streamed.body.data = p.text || '';
        break;
      }
      const b = node('said');
      const body = document.createElement('div');
      body.className = 'md';
      body.innerHTML = md(p.text);
      b.append(node('who','abhed'), body);
      (turnEl || tx).appendChild(b);
      break;
    }

    case 'agent.reasoning': {
      // Minimised by default: the reply is the answer, the reasoning is why.
      // Anyone who wants it is one click away, and nobody has to scroll past it.
      //
      // This must NOT clear streamEl. Reasoning is recorded once the turn's
      // text is complete, so it arrives between the deltas and agent.message:
      // dropping the stream reference here made agent.message believe no bubble
      // existed and append the whole reply a second time.
      hideThinking();
      const think = node('think collapsed');
      const hdr = node('hdr');
      const caret = node('caret', '\u25be');
      const label = node('', 'reasoning');
      const size = node('', wordCount(p.text) + ' words');
      size.style.cssText = 'margin-left:auto;font-size:10px';
      hdr.append(caret, label, size);
      const body = node('body', p.text || '');
      think.append(hdr, body);
      hdr.setAttribute('role','button');
      hdr.setAttribute('tabindex','0');
      hdr.setAttribute('aria-expanded','false');
      const toggle = () => {
        think.dataset.pinned = '1';
        setCollapsed(think, !think.classList.contains('collapsed'));
      };
      hdr.onclick = toggle;
      hdr.onkeydown = e => {
        if(e.key === 'Enter' || e.key === ' '){ e.preventDefault(); toggle(); }
      };
      (turnEl || newTurn()).appendChild(think);
      break;
    }

    case 'action.requested': {
      hideThinking();
      // A tool call ends the current streamed reply.
      streamEl = null; streamBody = null;
      const wrap = node('call');
      const hdr = node('hdr');
      const caret = node('caret', '\u25be');
      const tool = node('tool', p.tool);
      const arg = node('arg', summarize(p.tool, p.args));
      const peek = node('peek');   // one-line result, shown only when collapsed
      hdr.append(caret, tool, arg, peek);
      wrap.appendChild(hdr);
      makeCollapsible(wrap, hdr);
      (turnEl || newTurn()).appendChild(wrap);
      if(p.call_id) calls.set(p.call_id, wrap);
      stats.tools[p.tool] = (stats.tools[p.tool] || 0) + 1;
      if(p.requires_approval) approval(p);
      break;
    }

    case 'observation': {
      const wrap = calls.get(p.call_id) || turnEl || newTurn();
      if(p.is_error) wrap.classList.add('err');

      let cls = 'out';
      if(p.is_error) cls += ' err';
      else if(ev.trust === 'untrusted') cls += ' untrusted';

      const body = node(cls);
      // Anything worth reading in full opens in the drawer rather than
      // stretching the conversation column.
      const owner = calls.get(p.call_id);
      const isFile = ['read','write','edit'].includes(p.tool);
      const long = (p.content || '').split('\n').length > 12;
      if((isFile || long) && !p.is_error){
        const b = document.createElement('button');
        b.className = 'openfile'; b.type = 'button';
        b.textContent = isFile ? 'Open file' : 'View output';
        const label = owner ? (owner.querySelector('.arg') || {}).textContent || p.tool : p.tool;
        b.onclick = () => openDrawer(label, p.tool, p.content || '', isFile);
        body.appendChild(b);
      }
      if(ev.trust === 'untrusted' && !p.is_error){
        // Provenance is a first-class concept in Abhed: tool output is data,
        // never instruction. Saying so in the UI keeps that visible.
        body.appendChild(node('tag','untrusted data'));
      }
      body.appendChild(document.createTextNode(clip(p.content || '', 4000)));
      wrap.appendChild(body);

      if(live) showThinking('working');
      if(p.duration_ms !== undefined){
        const hdr = wrap.querySelector('.hdr');
        if(hdr && !hdr.querySelector('.ms')){
          hdr.appendChild(node('ms', p.duration_ms + 'ms'));
        }
      }
      // The call is finished, so its output stops being the thing you are
      // waiting on and becomes reference. Collapse it to the header and put a
      // one-line summary there, unless it failed - an error is exactly what
      // the reader needs to see without hunting for it.
      if(wrap.classList.contains('call') && !p.is_error){
        setPeek(wrap, p.content || '');
        collapse(wrap, true);
      }
      break;
    }

    case 'action.approved': {
      resolveApproval(p.call_id, 'approved', 'ok');
      break;
    }

    case 'action.denied': {
      resolveApproval(p.call_id, 'rejected', 'no');
      const wrap = calls.get(p.call_id) || turnEl || newTurn();
      wrap.classList.add('err');
      wrap.appendChild(node('out err', 'denied — ' + (p.reason || 'no reason given')));
      break;
    }

    case 'compaction.completed': {
      stats.compactions++;
      tx.appendChild(node('note',
        'context compacted · ' + p.before_tokens + ' → ' + p.after_tokens + ' tokens'));
      turnEl = null;
      break;
    }

    case 'session.ended': {
      hideThinking();
      live = false;
      // The session is over, so every remaining card is unanswerable. Leaving
      // them clickable is what made a reopened session show a dead approval
      // prompt that swallowed every click.
      approvals.forEach((_, id) => resolveApproval(id, 'not answered'));
      $('stop').hidden = true;
      stats.reason = p.reason;
      stats.turns = p.turns || stats.turns;
      stats.tin = p.tokens_in || stats.tin;
      stats.tout = p.tokens_out || stats.tout;
      stats.cached = p.tokens_cached || stats.cached;
      stats.compactions = p.compactions || stats.compactions;
      stats.ctx = p.context_tokens || stats.ctx;
      stats.ctxWindow = p.context_window || stats.ctxWindow;

      const n = node('note');
      n.append(kv('ended', p.reason), kv('turns', p.turns));

      // Context first, because it is the number that answers "how much room is
      // left". tokens_in beside it is a running total across every turn, so it
      // grows forever and looked like a session filling up when it was not.
      if (p.context_tokens) {
        n.append(kv('context', p.context_window
          ? p.context_tokens.toLocaleString() + ' / ' + p.context_window.toLocaleString() +
            ' (' + Math.round(p.context_tokens / p.context_window * 100) + '%)'
          : p.context_tokens.toLocaleString()));
      }
      n.append(kv('total cost', (p.tokens_in||0).toLocaleString() + ' in / ' +
                                (p.tokens_out||0).toLocaleString() + ' out'));
      tx.appendChild(n);
      refresh();
      break;
    }
  }
  tx.scrollTop = tx.scrollHeight;
}

function kv(k, v){
  const s = document.createElement('span');
  s.append(document.createTextNode(k + ' '), Object.assign(document.createElement('b'),
    {textContent: String(v)}));
  return s;
}

function summarize(tool, args){
  if(!args) return '';
  let a = args;
  if(typeof a === 'string'){ try{ a = JSON.parse(a); }catch{ return ''; } }
  switch(tool){
    case 'bash': return a.description || a.command || '';
    case 'read': case 'write': case 'edit': return shortPath(a.path || '');
    case 'glob': return a.pattern || '';
    case 'grep': return JSON.stringify(a.pattern || '');
    case 'search': return a.query || '';
    case 'task': return a.description || '';
    default: return a.path || a.pattern || a.query || a.description || '';
  }
}

function shortPath(p){
  const parts = String(p).split('/');
  return parts.length > 3 ? '…/' + parts.slice(-2).join('/') : p;
}

function clip(s, n){ return s.length > n ? s.slice(0,n) + '\n… ' + (s.length-n) + ' more characters' : s; }

// lastStreamedBubble finds the reply bubble the deltas were writing into, so
// agent.message can reconcile with it even when the stream reference is gone.
function lastStreamedBubble(){
  const scope = turnEl || $('tx');
  if(!scope) return null;
  const bubbles = scope.querySelectorAll('.said:not(.user)');
  const el = bubbles[bubbles.length - 1];
  if(!el) return null;
  // The text node after the 'who' label is what the deltas appended to.
  const body = [...el.childNodes].find(n => n.nodeType === 3);
  return body ? {el, body} : null;
}

// md renders a model's reply.
//
// Models write markdown whether asked to or not, so inserting the text raw
// shows the reader the asterisks and a table drawn in pipes. This handles what
// they actually emit and escapes everything first: a transcript is a record of
// untrusted content, and a reply that read a hostile file must not be able to
// put markup into this page.
function md(text){
  const esc = s => s.replace(/[&<>"']/g, c =>
    ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));

  const blocks = [];
  // Fenced code first, so nothing inside one is treated as markup.
  const F = String.fromCharCode(96).repeat(3);   // a fence, unwritable here
  const fenceRe = new RegExp(F + '(\\w*)\\n([\\s\\S]*?)' + F, 'g');
  text = text.replace(fenceRe, (_, lang, body) => {
    blocks.push('<pre class="code">' + esc(body.replace(/\n$/,'')) + '</pre>');
    return '\u0000' + (blocks.length - 1) + '\u0000';
  });

  const inline = s => esc(s)
    .replace(new RegExp(String.fromCharCode(96) + '([^' + String.fromCharCode(96) + ']+)' + String.fromCharCode(96), 'g'), '<code>$1</code>')
    .replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>')
    .replace(/(^|[^*])\*([^*\n]+)\*/g, '$1<i>$2</i>')
    .replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" rel="noopener noreferrer" target="_blank">$1</a>');

  const out = [];
  const lines = text.split('\n');
  let list = null, table = null;

  const closeList = () => { if(list){ out.push('</'+list+'>'); list = null; } };
  const closeTable = () => {
    if(!table) return;
    const rows = table.filter(r => !/^[\s|:-]+$/.test(r));
    out.push('<table>' + rows.map((r, i) => {
      const cells = r.replace(/^\||\|$/g,'').split('|').map(c => inline(c.trim()));
      const tag = i === 0 ? 'th' : 'td';
      return '<tr>' + cells.map(c => '<' + tag + '>' + c + '</' + tag + '>').join('') + '</tr>';
    }).join('') + '</table>');
    table = null;
  };

  for(const raw of lines){
    const line = raw.trimEnd();
    if(/^\s*\|.*\|\s*$/.test(line)){ closeList(); (table = table || []).push(line.trim()); continue; }
    closeTable();

    const h = /^(#{1,6})\s+(.*)$/.exec(line);
    if(h){
      closeList();
      const lvl = Math.min(h[1].length + 2, 6);
      out.push('<h' + lvl + '>' + inline(h[2]) + '</h' + lvl + '>');
      continue;
    }

    const ul = /^\s*[-*+]\s+(.*)$/.exec(line);
    if(ul){ if(list !== 'ul'){ closeList(); out.push('<ul>'); list = 'ul'; } out.push('<li>'+inline(ul[1])+'</li>'); continue; }

    const ol = /^\s*(\d+)\.\s+(.*)$/.exec(line);
    if(ol){ if(list !== 'ol'){ closeList(); out.push('<ol>'); list = 'ol'; } out.push('<li>'+inline(ol[2])+'</li>'); continue; }

    closeList();
    if(line.trim() === ''){ out.push(''); continue; }
    if(/^(-{3,}|\*{3,}|_{3,})$/.test(line.trim())){ out.push('<hr>'); continue; }
    out.push('<p>'+inline(line)+'</p>');
  }
  closeList(); closeTable();

  let html = out.join('');
  html = html.replace(/\u0000(\d+)\u0000/g, (_, i) => blocks[Number(i)]);
  return html;
}

function wordCount(s){ return String(s || '').trim().split(/\s+/).filter(Boolean).length; }

/* --------------------------------------------------------------- collapsing */

// makeCollapsible turns a header into a disclosure toggle for its block.
//
// A tool call is interesting while it runs and clutter once it has finished:
// after twenty calls the answer is far off-screen. So a completed call keeps
// only its header, and one click brings the output back. The state is per
// block and never automatic after the first collapse - reopening something and
// having it shut itself again is worse than never collapsing it.
function makeCollapsible(wrap, hdr){
  hdr.setAttribute('role','button');
  hdr.setAttribute('tabindex','0');
  hdr.setAttribute('aria-expanded','true');
  const toggle = () => {
    // A manual toggle is sticky: later events must not override the choice.
    wrap.dataset.pinned = '1';
    setCollapsed(wrap, !wrap.classList.contains('collapsed'));
  };
  hdr.onclick = e => {
    // The drawer button and any other control inside the header keep their
    // own behaviour rather than toggling the block.
    if(e.target.closest('button') && !e.target.closest('.hdr[role=button]') ) return;
    if(e.target.tagName === 'BUTTON') return;
    toggle();
  };
  hdr.onkeydown = e => {
    if(e.key === 'Enter' || e.key === ' '){ e.preventDefault(); toggle(); }
  };
}

// setCollapsed applies the state unconditionally. Use it for a user's own click.
function setCollapsed(wrap, on){
  wrap.classList.toggle('collapsed', on);
  const hdr = wrap.querySelector('.hdr');
  if(hdr) hdr.setAttribute('aria-expanded', String(!on));
}

// collapse is the automatic path: it yields to a choice the user already made,
// so a block you expanded does not shut itself when the next event arrives.
function collapse(wrap, on){
  if(wrap.dataset.pinned === '1') return;
  setCollapsed(wrap, on);
}

// setPeek puts the first meaningful line of the result in the header, so a
// collapsed call still says what happened.
function setPeek(wrap, content){
  const el = wrap.querySelector('.peek');
  if(!el) return;
  const first = String(content).split('\n').map(l => l.trim()).find(l => l) || '';
  el.textContent = first ? '· ' + clip(first, 80).split('\n')[0] : '';
}

/* ------------------------------------------------------------------ approvals */
function approval(p){
  const card = node('approve');
  const h = document.createElement('h4');
  h.textContent = 'Approval required — ' + p.tool;
  card.appendChild(h);
  if(p.reason) card.appendChild(Object.assign(document.createElement('p'), {textContent: p.reason}));

  const pre = document.createElement('pre');
  try{
    pre.textContent = JSON.stringify(
      typeof p.args === 'string' ? JSON.parse(p.args) : p.args, null, 2);
  }catch{ pre.textContent = String(p.args); }
  card.appendChild(pre);

  const row = node('row');
  const yes = Object.assign(document.createElement('button'), {className:'yes', textContent:'Approve'});
  const no  = Object.assign(document.createElement('button'), {className:'no',  textContent:'Reject'});
  // "Always allow" carries the policy scope back so default mode stops
  // re-prompting for the same kind of call this session — the console
  // equivalent of the CLI's [A] option. Only shown when policy suggests a
  // scope narrow enough to be safe to remember.
  const always = p.scope
    ? Object.assign(document.createElement('button'),
        {className:'no', textContent:'Always allow', title: p.scope})
    : null;
  const buttons = always ? [yes, no, always] : [yes, no];
  const decide = (ok, scope) => async () => {
    buttons.forEach(b => { b.disabled = true; });
    try{
      const body = scope ? {approved: ok, scope} : {approved: ok};
      await api('/v1/sessions/' + current + '/approve',
        {method:'POST', body: JSON.stringify(body)});
      if(scope) resolveApproval(p.call_id, 'always allowed', 'ok', scope);
      else resolveApproval(p.call_id, ok ? 'approved' : 'rejected', ok ? 'ok' : 'no');
    }catch(e){
      // A 409 means the session already moved on — the decision was made
      // elsewhere, or this is a replay of a finished session. Say so and
      // retire the card; re-enabling the buttons would invite a click that
      // can never succeed.
      const stale = /no approval is pending|session not found/i.test(e.message);
      if(stale){
        resolveApproval(p.call_id, 'no longer awaiting a decision');
        return;
      }
      card.appendChild(node('note', e.message));
      buttons.forEach(b => { b.disabled = false; });
    }
  };
  yes.onclick = decide(true); no.onclick = decide(false);
  if(always) always.onclick = decide(true, p.scope);
  row.append(...buttons);
  card.appendChild(row);
  // Attach the prompt to the tool row it is about, not the bottom of the
  // transcript, so the buttons — and later the verdict — sit with the command.
  const host = (p.call_id && calls.get(p.call_id)) || $('tx');
  host.appendChild(card);
  if(p.call_id) approvals.set(p.call_id, card);
}

// resolveApproval replaces a pending card with its outcome.
//
// An approval is answerable exactly once, while the session that raised it is
// still running. Replaying a finished session re-delivers the original
// action.requested, so without this the UI rebuilt a live-looking prompt for a
// decision made yesterday: clicking it POSTed to a session with nothing
// pending, the server answered 409, and the card sat there absorbing clicks.
function resolveApproval(callID, outcome, kind, title){
  // Only calls that actually raised a prompt have a card. Read-only tools are
  // auto-allowed and still emit action.approved, so without this guard every
  // read row would get a redundant "approved" tag — the clutter we are fixing.
  const card = approvals.get(callID);
  if(!card) return;
  approvals.delete(callID);
  if(card.isConnected) card.remove();
  // Prefer a pill on the tool row's header: it names the decision next to the
  // command and survives the row collapsing. Only when there is no row to pin
  // it to does it fall back to a standalone note.
  const row = calls.get(callID);
  const hdr = row && row.classList.contains('call') ? row.querySelector('.hdr') : null;
  if(hdr){
    if(!hdr.querySelector('.verdict')){
      const pill = node('verdict' + (kind ? ' ' + kind : ''), outcome);
      if(title) pill.title = title;
      hdr.appendChild(pill);
    }
  }else{
    tx.appendChild(node('note', outcome));
  }
}

/* ------------------------------------------------------------------ actions */
$('go').onclick = send;

// The first message opens a session; every later one continues it. That
// distinction is what makes "now add a test for it" resolve against what came
// before, instead of starting a fresh conversation each time.
async function send(){
  let prompt = $('q').value.trim();
  // A message that is only attachments is a reasonable thing to send: the
  // question is implied by the file.
  if(!prompt && !pending.length) return;
  $('go').disabled = true;
  try{
    if(current && !live){
      // Uploads need a session to belong to, which this turn already has.
      const paths = await flushUploads(current);
      await api('/v1/sessions/' + current + '/messages',
        {method:'POST', body: JSON.stringify({prompt: withFiles(prompt, paths)})});
      $('q').value = ''; autogrow();
      live = true;
      showThinking('waiting for the model');
      if(!es) connect(current);
    }else{
      // Files are staged before the session exists, so the first message
      // already names them. Creating a placeholder session to hold them
      // instead meant the UI opened its event stream on a turn that
      // immediately ended, and the real answer never rendered.
      const paths = await flushUploads(null);
      const r = await api('/v1/sessions',
        {method:'POST', body: JSON.stringify({prompt: withFiles(prompt, paths), mode: $('mode').value})});
      $('q').value = ''; autogrow();
      openSession(r.session_id, 'running');
      showThinking('waiting for the model');
    }
  }catch(e){
    $('tx').appendChild(node('note', e.message));
  }finally{
    $('go').disabled = false;
    $('q').focus();
  }
}

// ---------------------------------------------------------------- attachments
//
// Files are uploaded when the message is sent, not when they are picked: a
// file chosen and then removed should never have touched the server.
let pending = [];   // {file, name, note}

function renderFiles(){
  const box = $('files');
  box.textContent = '';
  pending.forEach((p, i) => {
    const chip = node('chipf', '');
    if(p.note) chip.classList.add('bad');
    const b = document.createElement('b');
    b.textContent = p.name;
    chip.appendChild(b);
    if(p.note){
      const n = document.createElement('span');
      n.textContent = p.note;
      chip.appendChild(n);
    }
    const x = document.createElement('span');
    x.className = 'x'; x.textContent = '×';
    x.title = 'Remove';
    x.onclick = () => { pending.splice(i, 1); renderFiles(); };
    chip.appendChild(x);
    box.appendChild(chip);
  });
}

// flushUploads sends every queued file and returns the paths the agent can
// read. A null session id stages the files, for a chat that does not exist yet.
async function flushUploads(sessionID){
  const paths = [];
  const url = sessionID ? '/v1/sessions/' + sessionID + '/upload' : '/v1/uploads';
  for(const p of pending){
    if(p.note){
      throw new Error(p.name + ' was not sent: ' + p.note);
    }
    const fd = new FormData();
    fd.append('file', p.file, p.name);
    const r = await fetch(url, {method:'POST', body: fd});
    const body = await r.json();
    if(!r.ok){
      throw new Error('upload failed for ' + p.name + ': ' + (body.error || r.status));
    }
    paths.push(body);
  }
  pending = [];
  renderFiles();
  return paths;
}

// withFiles names the uploaded paths in the prompt. The agent reads them with
// the ordinary read tool, so there is no second route for untrusted bytes into
// the context — the file is data the agent chooses to open, like any other.
function withFiles(prompt, uploaded){
  if(!uploaded.length) return prompt;
  const lines = uploaded.map(u => {
    let s = '- ' + u.path;
    if(u.kind) s += ' (' + u.kind + ')';
    if(u.note) s += ' — note: ' + u.note;
    return s;
  });
  const header = uploaded.length === 1 ? 'Attached file:' : 'Attached files:';
  return (prompt ? prompt + '\n\n' : 'Read the attached file(s).\n\n') +
         header + '\n' + lines.join('\n');
}



$('attach').onclick = () => $('file').click();
$('file').onchange = e => {
  for(const f of e.target.files){
    // Refuse oversize files here rather than after a slow upload.
    const note = f.size > 32 * 1024 * 1024 ? 'too large (32 MB limit)' : '';
    pending.push({file: f, name: f.name, note});
  }
  e.target.value = '';   // so the same file can be picked again
  renderFiles();
};

/* The slide-over rail.
 *
 * State lives in one class on <body> so CSS owns the animation and JS only
 * says open or closed. The rail must close whenever it has done its job —
 * picking a chat, starting a new one — or on a phone it stays sitting on top
 * of the conversation the user just asked to see. */
function setRail(open){
  document.body.classList.toggle('rail-open', open);
  const t = $('railtoggle');
  if(t){
    t.setAttribute('aria-expanded', open ? 'true' : 'false');
    t.setAttribute('aria-label', open ? 'Hide chats' : 'Show chats');
  }
  const s = $('scrim');
  if(s) s.hidden = !open;
}
const railOpen = () => document.body.classList.contains('rail-open');

if($('railtoggle')) $('railtoggle').onclick = () => setRail(!railOpen());
if($('scrim')) $('scrim').onclick = () => setRail(false);

// Escape closes the rail before anything else acts on the key, matching how
// every other dismissible layer on the platform behaves.
document.addEventListener('keydown', e => {
  if(e.key === 'Escape' && railOpen()){ setRail(false); e.stopPropagation(); }
}, true);

// Rotating to landscape turns the slide-over back into a column; a rail left
// "open" would then be stuck behind a scrim that is no longer visible.
window.addEventListener('resize', () => {
  if(window.innerWidth > 760 && railOpen()) setRail(false);
});

function newChat(){
  setRail(false);
  if(es){ es.close(); es = null; }
  current = null; live = false; lastSeq = 0; turnEl = null;
  calls.clear();
  approvals.clear();
  pending = []; renderFiles();
  Object.assign(stats, {turns:0, tin:0, tout:0, cached:0, tools:{}, reason:null, compactions:0, ctx:0, ctxWindow:0});
  $('sid').textContent = 'new chat';
  $('stop').hidden = true;
  $('wbfiles').hidden = $('wbchanges').hidden = true;
  closeDrawer();
  drawEmpty();
  refresh();
  $('q').focus();
}
$('new').onclick = newChat;

// The textarea grows with its content, like every chat input people know.
function autogrow(){
  const t = $('q');
  t.style.height = 'auto';
  t.style.height = Math.min(t.scrollHeight, 180) + 'px';
}
$('q').addEventListener('input', autogrow);

/* ---------------------------------------------------------------- drawer */
function openDrawer(name, kind, body, numbered){
  leaveWorkbench();
  $('dname').textContent = name;
  $('dkind').textContent = kind || '';
  const pre = document.createElement('pre');
  if(numbered){
    // read() returns numbered lines; keep the gutter separate so the code
    // itself stays selectable and copyable.
    for(const line of body.split('\n')){
      const m = line.match(/^\s*(\d+)\t(.*)$/);
      if(m){
        const g = document.createElement('span');
        g.className = 'ln'; g.textContent = m[1];
        pre.append(g, document.createTextNode(m[2] + '\n'));
      }else{
        pre.append(document.createTextNode(line + '\n'));
      }
    }
  }else{
    pre.textContent = body;
  }
  const host = $('dbody');
  host.textContent = '';
  host.appendChild(pre);
  document.querySelector('.shell').classList.add('open');
  $('drawer').setAttribute('aria-hidden','false');
}

function closeDrawer(){
  leaveWorkbench();
  document.querySelector('.shell').classList.remove('open');
  $('drawer').setAttribute('aria-hidden','true');
}
$('dclose').onclick = closeDrawer;
document.addEventListener('keydown', e => { if(e.key === 'Escape') closeDrawer(); });

/* ------------------------------------------------------------- workbench */
// The workspace as the agent left it: the tree, one file, what changed.
// Read-only. A file's content is whatever the agent or a cloned repository
// put there, so it only ever reaches the page as text nodes.
const wb = {open:false, tab:'files', timer:null};

function wbURL(what, path){
  return '/v1/sessions/' + encodeURIComponent(current) + '/' + what +
    (path === undefined ? '' : '?path=' + encodeURIComponent(path));
}

function leaveWorkbench(){
  wb.open = false;
  clearTimeout(wb.timer);
  $('wbtabs').hidden = true;
  document.querySelector('.shell').classList.remove('wide');
}

function openWorkbench(tab){
  if(!current) return;
  wb.open = true; wb.tab = tab;
  $('dname').textContent = 'Workspace';
  $('dkind').textContent = '';
  $('wbtabs').hidden = false;
  $('tabfiles').setAttribute('aria-selected', String(tab === 'files'));
  $('tabchanges').setAttribute('aria-selected', String(tab === 'changes'));

  const frame = node('wb'), side = node('wb-side'), main = node('wb-main');
  frame.id = 'wb'; side.id = 'wbside'; main.id = 'wbmain';
  frame.append(side, main);
  main.appendChild(node('wb-note', tab === 'files'
    ? 'Pick a file to read it.' : 'Pick a file to see what the agent changed.'));
  const host = $('dbody');
  host.textContent = '';
  host.appendChild(frame);
  document.querySelector('.shell').classList.add('open', 'wide');
  $('drawer').setAttribute('aria-hidden', 'false');
  if(tab === 'files') loadDir('', side, 0); else loadChanges();
}
$('wbfiles').onclick = () => openWorkbench('files');
$('wbchanges').onclick = () => openWorkbench('changes');
$('tabfiles').onclick = () => openWorkbench('files');
$('tabchanges').onclick = () => openWorkbench('changes');
$('wbreload').onclick = () => openWorkbench(wb.tab);

// While the agent works, the list of changes goes stale with every edit.
// Reloading on a tool result keeps it honest; the delay folds a burst of
// edits into one request.
function workbenchSaw(ev){
  if(!wb.open || wb.tab !== 'changes') return;
  if(ev.type !== 'observation' && ev.type !== 'session.ended') return;
  clearTimeout(wb.timer);
  wb.timer = setTimeout(() => { if(wb.open && wb.tab === 'changes') loadChanges(); }, 600);
}

function wbRow(depth, twisty, name){
  const b = document.createElement('button');
  b.type = 'button'; b.className = 'wb-row';
  b.style.paddingLeft = (10 + depth * 12) + 'px';
  const tw = document.createElement('span'); tw.className = 'tw'; tw.textContent = twisty;
  const nm = document.createElement('span'); nm.className = 'nm'; nm.textContent = name;
  b.append(tw, nm);
  return b;
}

function wbSelect(row){
  for(const el of document.querySelectorAll('#wbside .wb-row[aria-current]')) el.removeAttribute('aria-current');
  row.setAttribute('aria-current', 'true');
}

// One directory per request, fetched when it is first opened.
async function loadDir(path, host, depth){
  const session = current;
  let listing;
  try{ listing = await api(wbURL('tree', path)); }
  catch(e){ host.appendChild(node('wb-note', e.message)); return; }
  if(session !== current || !host.isConnected) return;
  if(!listing.entries.length && depth === 0) host.appendChild(node('wb-note', 'The workspace is empty.'));
  for(const e of listing.entries){
    const row = wbRow(depth, e.dir ? '▸' : '', e.name);
    host.appendChild(row);
    if(!e.dir){
      row.title = e.path + ' · ' + fmtSize(e.size);
      row.onclick = () => { wbSelect(row); viewFile(e.path); };
      continue;
    }
    const kids = node('');
    kids.hidden = true;
    host.appendChild(kids);
    let loaded = false;
    row.setAttribute('aria-expanded', 'false');
    row.onclick = () => {
      kids.hidden = !kids.hidden;
      row.firstChild.textContent = kids.hidden ? '▸' : '▾';
      row.setAttribute('aria-expanded', String(!kids.hidden));
      if(!loaded){ loaded = true; loadDir(e.path, kids, depth + 1); }
    };
  }
  if(listing.truncated){
    const note = node('wb-note', 'Only the first ' + listing.entries.length + ' entries are listed.');
    note.style.paddingLeft = (10 + depth * 12) + 'px';
    host.appendChild(note);
  }
}

function fmtSize(n){
  if(n < 1024) return n + ' B';
  if(n < 1048576) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1048576).toFixed(1) + ' MB';
}

// wbShow puts a titled view in the main pane and returns the element to fill.
function wbShow(name, meta){
  const main = $('wbmain');
  if(!main) return null;
  main.textContent = '';
  const bar = node('wb-bar');
  const back = document.createElement('button');
  back.type = 'button'; back.className = 'ghost wb-back'; back.textContent = '← Back';
  back.onclick = () => $('wb').classList.remove('viewing');
  const nm = document.createElement('span'); nm.className = 'nm'; nm.textContent = name; nm.title = name;
  const mt = document.createElement('span'); mt.textContent = meta || '';
  bar.append(back, nm, mt);
  const view = node('wb-view');
  main.append(bar, view);
  $('wb').classList.add('viewing');
  return view;
}

async function viewFile(path){
  const session = current;
  let f;
  try{ f = await api(wbURL('file', path)); }
  catch(e){
    const view = wbShow(path, '');
    if(view) view.appendChild(node('wb-note', e.message));
    return;
  }
  if(session === current) showFile(f);
}

function showFile(f){
  const view = wbShow(f.path, fmtSize(f.size));
  if(!view) return;
  if(f.binary){
    view.appendChild(node('wb-note', 'Binary file. It is not shown here.'));
    return;
  }
  if(f.truncated){
    view.appendChild(node('wb-note', 'This file is ' + fmtSize(f.size) +
      '. Only the first ' + fmtSize(f.content.length) + ' are shown.'));
  }
  const pre = document.createElement('pre');
  const lines = f.content.split('\n');
  if(lines.length > 1 && lines[lines.length - 1] === '') lines.pop();
  lines.forEach((line, i) => {
    const g = document.createElement('span');
    g.className = 'ln'; g.textContent = String(i + 1);
    pre.append(g, document.createTextNode(line + '\n'));
  });
  view.appendChild(pre);
}

async function loadChanges(){
  const session = current;
  const side = $('wbside');
  if(!side) return;
  let res;
  try{ res = await api(wbURL('changes')); }
  catch(e){ side.textContent = ''; side.appendChild(node('wb-note', e.message)); return; }
  if(session !== current || !side.isConnected) return;
  const selected = (side.querySelector('.wb-row[aria-current]') || {}).title;
  side.textContent = '';
  if(!res.available){
    side.appendChild(node('wb-note', 'Changes are kept while a session is live on this ' +
      'server. This one was opened from its record, so there is nothing to compare against.'));
    return;
  }
  if(!res.files.length){
    side.appendChild(node('wb-note', 'The agent has not changed any files in this session.'));
    return;
  }
  for(const f of res.files){
    const mark = {added:'A', deleted:'D'}[f.status] || 'M';
    const row = wbRow(0, mark, f.path);
    row.title = f.path;
    const ct = document.createElement('span'); ct.className = 'ct';
    const plus = document.createElement('span'); plus.className = 'plus'; plus.textContent = '+' + f.added;
    const minus = document.createElement('span'); minus.className = 'minus'; minus.textContent = '−' + f.removed;
    ct.append(plus, document.createTextNode(' '), minus);
    row.appendChild(ct);
    row.onclick = () => { wbSelect(row); viewDiff(f); };
    side.appendChild(row);
    // A reload keeps the file being read open, with its diff brought up to date.
    if(f.path === selected){ wbSelect(row); viewDiff(f); }
  }
}

function viewDiff(f){
  const view = wbShow(f.path, f.status);
  if(!view) return;
  if(!f.diff){
    view.appendChild(node('wb-note', f.note ? 'No diff: ' + f.note + '.' : 'No textual change.'));
    return;
  }
  const box = node('diff');
  f.diff.replace(/\n$/, '').split('\n').forEach((line, i) => {
    // The two header lines are told apart by position: a removed line that
    // itself starts with "--" looks exactly like one.
    box.appendChild(node(i < 2 ? 'meta' : diffClass(line), line));
  });
  view.appendChild(box);
}

function diffClass(line){
  if(line.startsWith('\\')) return 'meta';
  if(line.startsWith('@@')) return 'hunk';
  if(line.startsWith('+')) return 'add';
  if(line.startsWith('-')) return 'del';
  return '';
}

// The empty state is authored once, in the page markup, and captured here so
// "New chat" restores exactly what the page loaded with — the same mark, the
// same words — rather than a second copy that drifts.
const EMPTY_HTML = (document.querySelector('#tx .empty') || {}).innerHTML || '';
function drawEmpty(){
  const tx = $('tx');
  tx.textContent = '';
  const wrap = node('empty');
  wrap.innerHTML = EMPTY_HTML;
  tx.appendChild(wrap);
  drawExamples();
}

$('q').addEventListener('keydown', e => {
  if(e.key === 'Enter' && !e.shiftKey){ e.preventDefault(); send(); }
});

$('stop').onclick = async () => {
  if(!current) return;
  try{ await api('/v1/sessions/' + current + '/interrupt', {method:'POST'}); }catch{}
};

// Show who is signed in when authentication is configured. A 401 simply means
// this deployment runs without it, which is a valid single-tenant setup.
async function whoami(){
  let me = null;
  try{ me = await api('/v1/whoami'); }catch{}

  if(!me || !me.authenticated){
    // No signed-in user. REMOVE the chip rather than hiding it: a hidden
    // control is still in the document, and a Sign out link that leads
    // nowhere is worse than no link at all. This is what produced a 404
    // when auth was never configured.
    const box = $('whobox');
    if(box && box.parentNode) box.parentNode.removeChild(box);
    return;
  }

  $('who').textContent = me.email || me.name || me.subject;
  // Only links this deployment can answer: a Switch with no route behind it
  // was a 404 on local accounts.
  if(me.switch_url){ $('switchuser').href = me.switch_url; $('switchuser').hidden = false; }
  if(me.password_url){ $('pwlink').href = me.password_url; $('pwlink').hidden = false; }
  // Behind a proxy, sign-out is the proxy's, and offered only when configured.
  if(me.sign_out_url){ $('signout').href = me.sign_out_url; $('signout').hidden = false; }
  try{
    if(sessionStorage.getItem('abhed.must_change') === '1'){
      sessionStorage.removeItem('abhed.must_change');
      note(me.password_url ? 'This password was set for you. Change it under Password, at the top right.'
                           : 'This password was set for you. Ask your administrator to change it.');
    }
  }catch{}
  $('who').title = 'tenant ' + me.tenant +
    (me.groups && me.groups.length ? ' · ' + me.groups.join(', ') : '');
  $('whobox').hidden = false;
}

// What this deployment can do, and whether the person here may administer
// it. Runs regardless of sign-in: the examples depend on which tools exist,
// not on who is looking. The server decides who is an admin; the UI only
// decides whether to draw the link. Flipping it in devtools yields a link to a
// page that answers 403, because every /admin route is guarded server-side.
async function capabilities(){
  try{
    const o = await api('/v1/overview');
    TOOLS = new Set(o && o.tools ? o.tools : []);
    // An admin page exists only in editions that serve one.
    const a = $('adminlink');
    if(a && o && o.admin && o.admin_url){ a.href = o.admin_url; a.hidden = false; }
  }catch{ TOOLS = new Set(); }
  drawExamples();
}

// A first-run console that only says "ask something" teaches nothing. These
// are the three shapes Abhed handles, so the examples double as documentation.
// Deliberately generic. The first example was "What is z/OS and where is it
// used?", which read as a product aimed at mainframe shops to everyone else —
// a first-run screen sets the expectation of what the tool is FOR, so a niche
// example narrows the product in the reader's mind before they have typed
// anything.
let TOOLS = null;
const EXAMPLES = [
  ['Understand code', 'What does the Valid function do in this codebase?', null],
  ['Make a change', 'The tests in pkg/auth are failing. Find the bug and fix it.', 'edit'],
  ['Research', 'Search for the CVEs published this month that affect OpenSSL 3 and tell me which ones matter on Debian 12.', 'web_search'],
  ['Read a document', 'Read the attached design doc and list every external system it depends on.', 'read'],
  ['Explain a concept', 'Explain how TLS certificate validation works.', null],
  ['Operate', 'Which pods in the staging namespace have restarted in the last hour, and why?', 'k8s_get'],
];

function drawExamples(){
  const box = $('examples');
  if(!box) return;
  box.textContent = '';
  for(const [label, text, needs] of EXAMPLES){
    // Until the overview arrives, show the tool-free examples only.
    if(needs && (!TOOLS || !TOOLS.has(needs))) continue;
    const b = document.createElement('button');
    b.className = 'chip';
    b.type = 'button';
    const strong = document.createElement('div');
    strong.className = 'lbl';
    strong.textContent = label;
    b.append(strong, document.createTextNode(text));
    b.onclick = () => { $('q').value = text; $('q').focus(); };
    box.appendChild(b);
  }
}

// A live indicator while the model is working. A cold 30B model can take ~30s
// for its first token, and a static line is indistinguishable from a hang.
function showThinking(what){
  hideThinking();
  const el = node('thinking');
  el.id = 'thinking';
  const bars = node('bars');
  for(let i=0;i<4;i++) bars.appendChild(document.createElement('i'));
  const label = document.createElement('span');
  label.textContent = what || 'thinking';
  const clock = document.createElement('span');
  clock.style.cssText = 'color:var(--muted);font-variant-numeric:tabular-nums';
  el.append(bars, label, clock);
  $('tx').appendChild(el);

  const t0 = Date.now();
  el.dataset.timer = setInterval(() => {
    clock.textContent = ((Date.now()-t0)/1000).toFixed(1) + 's';
  }, 100);
  $('tx').scrollTop = $('tx').scrollHeight;
}

function hideThinking(){
  const el = $('thinking');
  if(!el) return;
  clearInterval(Number(el.dataset.timer));
  el.remove();
}

drawExamples(); whoami(); capabilities(); health(); refresh(); loadProviders();
setInterval(health, 10000);
setInterval(refresh, 5000);
</script>
</body>
</html>`, "\x00", "")

// authDisabledHTML is shown when someone reaches /login or /logout on a server
// running without authentication. It says what is true and what to change,
// rather than leaving a 404 that looks like a fault.
const authDisabledHTML = `<!doctype html><meta charset="utf-8">
<title>Sign-in not configured</title>
<style>
:root{color-scheme:light dark}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0B0E13;
  color:#E8EDF4;font:14px/1.65 -apple-system,BlinkMacSystemFont,"Segoe UI",system-ui,sans-serif}
@media (prefers-color-scheme:light){body{background:#F5F7FA;color:#0F141B}}
.card{max-width:520px;padding:30px 34px;border-radius:12px;background:#141922;
  border:1px solid #252D3A}
@media (prefers-color-scheme:light){.card{background:#fff;border-color:#DCE3EC}}
h1{margin:0 0 10px;font-size:17px;display:flex;align-items:center;gap:9px}
svg{width:19px;height:19px;fill:#4C8FD6}
p{margin:0 0 12px;color:#8A96A8}
pre{background:#0F141C;border:1px solid #252D3A;border-radius:7px;padding:12px 14px;
  font:11.5px/1.6 ui-monospace,SFMono-Regular,Menlo,monospace;color:#BAC6D4;overflow-x:auto}
@media (prefers-color-scheme:light){pre{background:#EDF1F6;border-color:#DCE3EC;color:#3A4757}}
a{color:#4C8FD6}
</style>
<div class="card">
  <h1><svg viewBox="0 0 24 24" aria-hidden="true">
    <rect x="3" y="3" width="18" height="3" rx="1"/>
    <rect x="9" y="7.5" width="1.6" height="9" rx=".6" opacity=".85"/>
    <rect x="11.7" y="7.5" width="1.6" height="9" rx=".6"/>
    <rect x="14.4" y="7.5" width="1.6" height="9" rx=".6" opacity=".85"/>
    <rect x="3" y="18" width="18" height="3" rx="1"/></svg>
    Sign-in is not configured</h1>
  <p>This Abhed server runs with <code>auth.mode: none</code> — a single-tenant
     setup with no user accounts, so there is nobody to sign in or out as.</p>
  <p>To enable sign-in with local accounts, set the mode and issue an account:</p>
  <pre>{
  "auth": { "mode": "local" }
}

$ abhed user add alice -admin</pre>
  <p>See <code>docs/ops/enabling-auth.md</code>. Sign-in through an identity
     provider (OIDC) is part of the paid editions, Team and Enterprise.</p>
  <p><a href="/">← Back to Abhed</a></p>
</div>`
