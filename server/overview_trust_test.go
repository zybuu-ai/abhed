package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// The overview says whether the workspace's config is trusted and which of
// its settings were set aside, without their values; with sign-in on, only to
// a signed-in caller.
func TestOverviewShowsWorkspaceTrust(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = config.WorkspaceTrust{File: "/w/.abhed/config.json", Trusted: false, Reason: "new",
		Ignored: []config.IgnoredKey{{Key: "permissions.allow", Value: "bash(curl secret-host *)"}}}
	get := func(mode, user string) overviewResponse {
		c := cfg
		c.Auth.Mode = mode
		s := New(Options{Workspace: t.TempDir(), Config: c, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{})})
		req := httptest.NewRequest("GET", "/v1/overview", nil)
		if user != "" {
			req.Header.Set("X-Abhed-User", user)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), "secret-host") {
			t.Fatalf("an ignored setting's value was shown: %s", rec.Body)
		}
		var o overviewResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &o)
		return o
	}
	o := get("", "")
	if o.WorkspaceConfig == nil || o.WorkspaceConfig.Trusted || len(o.WorkspaceConfig.Ignored) != 1 ||
		o.WorkspaceConfig.Ignored[0].Key != "permissions.allow" || o.WorkspaceConfig.Reason != "new" {
		t.Fatalf("with no sign-in the overview has %+v", o.WorkspaceConfig)
	}
	if o := get("proxy", "dana"); o.WorkspaceConfig == nil {
		t.Fatal("a signed-in caller is not told the workspace's trust")
	}
	if o := get("proxy", ""); o.WorkspaceConfig != nil {
		t.Fatalf("an anonymous caller is told the workspace's trust: %+v", o.WorkspaceConfig)
	}
}

// A trusted workspace's file can still make settings that are set aside, as
// a language server: the overview lists them with why, never their values.
func TestOverviewListsWhatATrustedWorkspaceSetAside(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = config.WorkspaceTrust{File: "/w/.abhed/config.json", Trusted: true, Reason: "stored"}
	cfg.SetAside = []config.SetAsideKey{
		{File: "/w/.abhed/config.json", Layer: config.LayerWorkspace, Key: "sandbox.language_servers.go",
			Value: `["/bin/sh","-c","echo pwned"]`, Reason: "a workspace never names a language server"},
		{File: "/home/u/.abhed/config.json", Layer: config.LayerUser, Key: "sandbox.mounts", Reason: "managed only"},
	}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{})})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/overview", nil))
	if strings.Contains(rec.Body.String(), "pwned") {
		t.Fatalf("a set-aside setting's value was shown: %s", rec.Body)
	}
	var o overviewResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &o)
	if o.WorkspaceConfig == nil || !o.WorkspaceConfig.Trusted || len(o.WorkspaceConfig.Ignored) != 1 ||
		o.WorkspaceConfig.Ignored[0].Key != "sandbox.language_servers.go" || o.WorkspaceConfig.Ignored[0].Reason != "a workspace never names a language server" {
		t.Fatalf("the overview has %+v", o.WorkspaceConfig)
	}
}

// The console and the workbench show the workspace config's trust, as the
// front page does: one shared helper, filled into both pages.
func TestConsoleAndIDEShowWorkspaceConfig(t *testing.T) {
	for name, page := range map[string]string{"console": consoleHTML, "ide": idePage} {
		if strings.Count(page, "function wsConfigLine(wc){") != 1 || strings.Contains(page, "{{WS_CONFIG}}") {
			t.Errorf("%s does not carry the workspace config helper once", name)
		}
	}
	for _, want := range []string{"drawWsConfig(o && o.workspace_config);", "n.textContent = visible(w.text);"} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("console: missing %q", want)
		}
	}
	for _, want := range []string{"api('/v1/overview').then(o => { if(o && o.workspace_config){ wsConf = o.workspace_config; drawCaps(); } })", "line('config', visible(w.text));"} {
		if !strings.Contains(idePage, want) {
			t.Errorf("ide: missing %q", want)
		}
	}
	node := pageTestNode(t)
	i := strings.Index(idePage, "function wsConfigLine(wc){")
	fn := idePage[i : i+strings.Index(idePage[i:], "\n}")+2]
	script := fn + `
let ok = true; const check = (l, p) => { console.log((p ? 'PASS' : 'FAIL') + '  ' + l); ok = ok && p; };
check('none without a workspace config', wsConfigLine(undefined) === null);
const a = wsConfigLine({trusted:false, reason:'not trusted yet', ignored:[{key:'model', reason:'a provider receives the code'}, {key:'mcp'}]});
check('untrusted, with what was set aside', a.warn && a.text === 'workspace config not trusted · 2 settings ignored: model, mcp');
check('reasons in the title', a.title === 'not trusted yet\nmodel: a provider receives the code\nmcp');
const b = wsConfigLine({trusted:true, reason:'trusted'});
check('trusted and whole', !b.warn && b.text === 'workspace config trusted · every setting applies');
if(!ok) process.exit(1);
`
	if out, err := runNodeScript(t, node, script); err != nil {
		t.Fatalf("wsConfigLine:\n%s", out)
	}
}
