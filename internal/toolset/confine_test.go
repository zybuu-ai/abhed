package toolset

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	ConfineMCP(gw, config.Config{}, sb, nil)
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
	ConfineMCP(gw, cfg, sb, nil)
	for _, launch := range []mcp.Launcher{gw.Confine, gw.ConfineNet} {
		if launch == nil {
			t.Fatal("no launcher under the allowlist")
		}
		if _, err := launch(context.Background(), "s", []string{"/bin/echo"}, nil, nil); !errors.Is(err, mcp.ErrUnconfined) {
			t.Fatalf("an unconfinable server was not refused under the allowlist: %v", err)
		}
	}
}

// otherTier is a sandbox of a tier that has no way to confine a server.
type otherTier struct{ sandbox.Sandbox }

func (otherTier) Tier() sandbox.Tier { return sandbox.TierContainer }

// The fence, container and VM tiers start a server through their own
// launcher, whatever sandbox.network is; a tier with none refuses it, and a
// server set to host_files runs as the host's outside the allowlist.
func TestConfineMCPOnEveryTier(t *testing.T) {
	p := sandbox.DefaultPolicy(t.TempDir())
	for _, sb := range []sandbox.Sandbox{sandbox.NewFence(p), sandbox.NewContainer(p), sandbox.NewGVisor(p)} {
		if _, ok := sb.(sandbox.ServerLauncher); !ok {
			t.Errorf("the %s tier has no server launcher", sb.Tier())
		}
		confine, _ := serverLaunchers(sb, false)
		if confine == nil {
			t.Errorf("the %s tier starts a server unconfined", sb.Tier())
		}
	}
	gw := mcp.NewGateway()
	ConfineMCP(gw, config.Config{}, otherTier{}, nil)
	if gw.Confine == nil || gw.ConfineNet != nil {
		t.Fatal("a tier that cannot confine left a server unconfined, or host_files confined")
	}
	if _, err := gw.Confine(context.Background(), "s", []string{"/bin/echo"}, nil, nil); !errors.Is(err, mcp.ErrUnconfined) || !strings.Contains(err.Error(), "host_files") {
		t.Fatalf("not refused with the way out: %v", err)
	}
}

// On the none tier a stdio server runs as the host's, and the person is told
// so, with why, whether the tier is known at once or chosen later.
func TestConfineMCPWarnsOnTheNoneTier(t *testing.T) {
	none := sandbox.NewNone(sandbox.DefaultPolicy(t.TempDir()))
	for _, sb := range []sandbox.Sandbox{none, pending{Sandbox: none, chosen: none}, nil} {
		var got []string
		gw := mcp.NewGateway()
		ConfineMCP(gw, config.Config{}, sb, func(f string, a ...any) { got = append(got, fmt.Sprintf(f, a...)) })
		if gw.Confine == nil {
			t.Fatal("no launcher to warn from")
		}
		cmd, err := gw.Confine(context.Background(), "srv", []string{"/bin/echo"}, nil, nil)
		if err != nil || cmd.Path != "/bin/echo" {
			t.Fatalf("not run as the host's: %v %+v", err, cmd)
		}
		if len(got) != 1 || !strings.Contains(got[0], `mcp server "srv" runs unconfined`) || !strings.Contains(got[0], "host_files") {
			t.Fatalf("warnings: %q", got)
		}
		if sb != nil && !strings.Contains(got[0], "the sandbox tier is none: ") {
			t.Fatalf("no reason given: %q", got)
		}
		// The gateway keeps the mark for the panels that list its servers.
		if why := gw.Unconfined()["srv"]; why == "" || !strings.Contains(got[0], why) {
			t.Fatalf("not marked unconfined with the reason: %q", why)
		}
	}
	// Marked even with no one to warn.
	gw := mcp.NewGateway()
	ConfineMCP(gw, config.Config{}, none, nil)
	if _, err := gw.Confine(context.Background(), "quiet", []string{"/bin/echo"}, nil, nil); err != nil || gw.Unconfined()["quiet"] == "" {
		t.Fatalf("unmarked: %v %v", err, gw.Unconfined())
	}
}
