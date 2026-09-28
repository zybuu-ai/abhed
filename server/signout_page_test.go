package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every sign-out control runs its page's own script: a same-origin fetch POST
// to /logout, then a navigation to where the answer says. A plain form post
// arrives from Chrome as Origin: null, which is how every control once failed.
func TestPagesSignOutByFetch(t *testing.T) {
	node := pageTestNode(t)
	type page struct{ src, drive string }
	// A link click on the workbench and the console; a form submit on the others.
	click := `const ev = {currentTarget:{href:'/logout'}, prevented:false, preventDefault(){ this.prevented = true; }};
postSignOut(ev);`
	submit := `const ev = {prevented:false, preventDefault(){ this.prevented = true; }};
__handlers.so.submit(ev);`
	pages := map[string]page{
		"ide":      {src: ideHTML, drive: click},
		"console":  {src: consoleHTML, drive: click},
		"account":  {src: accountHTML, drive: submit},
		"sign-out": {src: signOutHTML, drive: submit},
	}
	for name, p := range pages {
		t.Run(name, func(t *testing.T) {
			var script string
			if p.drive == click {
				script = jsFunction(t, p.src, "function postSignOut(e){") + "\n" + jsFunction(t, p.src, "async function signOut(){")
			} else {
				i := strings.Index(p.src, "async function signOut(){")
				j := strings.Index(p.src, "document.getElementById('so').addEventListener('submit'")
				if i < 0 || j < i || !strings.Contains(p.src, `<form id="so" method="post" action="/logout">`) {
					t.Fatalf("the %s page has no scripted sign-out form", name)
				}
				script = p.src[i : j+strings.Index(p.src[j:], "\n")]
			}
			harness := `globalThis.__calls = []; globalThis.__handlers = {};
globalThis.location = {href:'http://h.test/page', origin:'http://h.test'};
globalThis.document = {getElementById: id => ({addEventListener: (type, f) => { (__handlers[id] = __handlers[id] || {})[type] = f; }})};
let __reply = {ok:true, json: async () => ({next:'https://idp.test/end'})};
globalThis.fetch = async (url, opts) => { __calls.push([url, opts]); if(__reply instanceof Error) throw __reply; return __reply; };
const tick = () => new Promise(r => setTimeout(r, 0));
`
			cases := `
let ok = true;
const check = (label, pass) => { console.log((pass ? 'PASS' : 'FAIL') + '  ' + label); ok = ok && pass; };
` + p.drive + `
await tick(); await tick();
check('the default navigation is prevented', ev.prevented);
check('one POST to /logout', __calls.length === 1 && __calls[0][0] === '/logout' && __calls[0][1].method === 'POST');
check('it asks for JSON', /application\/json/.test(__calls[0][1].headers.Accept));
check('it goes where the answer says', location.href === 'https://idp.test/end');
__calls.length = 0; location.href = 'http://h.test/page'; __reply = new Error('offline');
` + strings.Replace(p.drive, "const ev", "const ev2", 1) + `
await tick(); await tick();
check('a failed sign-out lands on the confirm page', location.href === '/logout');
if(!ok) process.exit(1);
`
			if out, err := runNodeScript(t, node, harness+script+"\n"+cases); err != nil {
				t.Fatalf("the %s page's sign-out failed:\n%s", name, out)
			}
		})
	}
}

// jsFunction returns one top-level function from a page's script.
func jsFunction(t *testing.T, src, head string) string {
	t.Helper()
	i := strings.Index(src, head)
	if i < 0 {
		t.Fatalf("%q is missing from the page", head)
	}
	return src[i : i+strings.Index(src[i:], "\n}\n")+3]
}

func pageTestNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("ABHED_REQUIRE_PAGE_TESTS") != "" {
			t.Fatal("node not installed")
		}
		t.Skip("node not installed; skipping page test")
	}
	return node
}

func runNodeScript(t *testing.T, node, script string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test.mjs"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "test.mjs")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	t.Log("\n" + strings.TrimSpace(string(out)))
	return string(out), err
}
