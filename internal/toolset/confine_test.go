package toolset

import (
	"context"
	"errors"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// pending is a sandbox chosen later, as the terminal CLI's is.
type pending struct {
	sandbox.Sandbox
	chosen sandbox.Sandbox
}

func (p pending) Resolve() (sandbox.Sandbox, error) { return p.chosen, nil }

// A sandbox still being chosen gives the gateway launchers that wait for the
// choice: a tier that cannot confine runs the server plainly, or refuses it
// under the allowlist.
func TestConfineMCPSeesThroughAPendingSandbox(t *testing.T) {
	none := sandbox.NewNone(sandbox.DefaultPolicy(t.TempDir()))
	sb := pending{Sandbox: none, chosen: none}

	gw := mcp.NewGateway()
	ConfineMCP(gw, config.Config{}, sb)
	if gw.Confine == nil || gw.ConfineNet != nil || gw.MustConfine {
		t.Fatalf("launchers outside the allowlist: confine %v, net %v, must %v", gw.Confine != nil, gw.ConfineNet != nil, gw.MustConfine)
	}
	cmd, err := gw.Confine(context.Background(), "s", []string{"/bin/echo", "hi"}, []string{"A=1"}, nil)
	if err != nil || cmd.Path != "/bin/echo" || len(cmd.Env) != 1 {
		t.Fatalf("not run plainly on the none tier: %v %+v", err, cmd)
	}

	var cfg config.Config
	cfg.Sandbox.Network = config.NetworkAllowlist
	gw = mcp.NewGateway()
	ConfineMCP(gw, cfg, sb)
	for _, launch := range []mcp.Launcher{gw.Confine, gw.ConfineNet} {
		if launch == nil {
			t.Fatal("no launcher under the allowlist")
		}
		if _, err := launch(context.Background(), "s", []string{"/bin/echo"}, nil, nil); !errors.Is(err, mcp.ErrUnconfined) {
			t.Fatalf("an unconfinable server was not refused under the allowlist: %v", err)
		}
	}
}
