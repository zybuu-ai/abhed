package server

import (
	"context"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// The operator's configuration attempts go into each session's record on a
// single-user server, and on a server accounts sign in to once into the
// admin audit instead of every user's record.
func TestOperatorConfigAttemptsGoWhereTheOperatorReadsThem(t *testing.T) {
	for _, c := range []struct {
		mode        string
		inSession   bool
		auditedOnce bool
	}{{"none", true, false}, {"local", false, true}} {
		t.Run(c.mode, func(t *testing.T) {
			cfg := config.Default()
			cfg.Auth.Mode = c.mode
			cfg.SetAside = []config.SetAsideKey{{File: "/home/op/.abhed/config.json", Layer: config.LayerUser, Key: "web_search.enabled", Value: "true"}}
			var audited []string
			s := New(Options{Workspace: tempDirResolved(t), Config: cfg, Adapter: stubAdapter{},
				Registry: tools.NewRegistry(tools.Read{}), Logger: discardLogger(),
				AdminAudit: func(_ context.Context, action, target string, _ map[string]any) {
					audited = append(audited, action+" "+target)
				}})
			for _, user := range []string{"local:ann", "local:bob"} {
				id, err := s.StartSession(context.Background(), StartSpec{Prompt: "hi", Tenant: "default", User: user})
				if err != nil {
					t.Fatal(err)
				}
				evs, err := s.store.Events(id)
				if err != nil {
					t.Fatal(err)
				}
				var refused int
				for _, e := range evs {
					if e.Type == agent.EvConfigRefused {
						refused++
					}
				}
				if (refused == 1) != c.inSession {
					t.Errorf("%s: %d config.refused in the session, want in session %v", user, refused, c.inSession)
				}
			}
			if want := []string{"config.refused web_search.enabled"}; c.auditedOnce != (len(audited) == 1 && audited[0] == want[0]) || (!c.auditedOnce && len(audited) != 0) {
				t.Errorf("audited %v", audited)
			}
		})
	}
}
