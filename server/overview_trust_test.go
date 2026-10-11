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
