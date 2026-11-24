package app

import (
	"context"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// A command that fails for the network is told to Studio with the choices;
// the person opens a host, which is kept and listed for the workspace and
// recorded, and revoking it applies to new chats. The first message carries
// the session's facts to the model.
func TestStudioNetworkOffer(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"mode":"bypass"}`,
		callTool("c1", "bash", map[string]any{"command": "printf 'curl: (6) Could not resolve host: github.com\\n'; exit 6", "description": "fetch"}),
		say("The network is off here."))
	id := r.open()
	from := r.cl.mark()
	r.prompt(id, "fetch github.com")
	offer := r.cl.waitFor(from, "the network offer", func(m rpcMessage) bool {
		return m.Method == "_abhed/sandbox/network_refused"
	})
	if p := string(offer.Params); !strings.Contains(p, `"canOpen":true`) || !strings.Contains(p, `"workspace","host","once"`) {
		t.Fatalf("offer %s", p)
	}
	r.model.mu.Lock()
	first := r.model.bodies[0]
	r.model.mu.Unlock()
	if !strings.Contains(first, "Session: True now") || !strings.Contains(first, "Network: off") {
		t.Fatalf("the first request has no session facts: %.400s", first)
	}

	r.cl.refused(errPolicy, "_abhed/sandbox/network", map[string]any{"sessionId": id, "scope": "host"})
	var got struct {
		Opened string `json:"opened"`
	}
	r.cl.ok("_abhed/sandbox/network", map[string]any{"sessionId": id, "scope": "host", "host": "GitHub.com"}, &got)
	if !strings.Contains(got.Opened, "github.com") || !strings.Contains(got.Opened, "kept") {
		t.Fatalf("opened %q", got.Opened)
	}
	if changed, _ := r.recorded(id, agent.EvPermissionChanged); len(changed) != 1 || changed[0]["rule"] != "network(github.com)" {
		t.Fatalf("recorded %v", changed)
	}
	var list struct {
		Grants []map[string]any `json:"grants"`
	}
	r.cl.ok("_abhed/grants/list", map[string]any{"sessionId": id}, &list)
	if len(list.Grants) != 1 || list.Grants[0]["id"] != "network" || list.Grants[0]["scope"] != "network(github.com)" {
		t.Fatalf("listed %v", list.Grants)
	}
	if k := keptNetwork(r.ws); len(k.Hosts) != 1 || k.Hosts[0] != "github.com" {
		t.Fatalf("kept %+v", k)
	}
	r.cl.ok("_abhed/grants/revoke", map[string]any{"sessionId": id, "id": "network"}, nil)
	if k := keptNetwork(r.ws); len(k.Hosts) != 0 || k.All {
		t.Fatalf("kept after revoke %+v", k)
	}
}

// Inside an agent's command, neither Studio's request nor /network opens the
// network; output that only prints the offer's words raises no offer.
func TestNetworkRefusedInsideAnAgentCommand(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"mode":"bypass"}`,
		callTool("c1", "bash", map[string]any{"command": "echo '" + toolset.NetworkOfferText + "'", "description": "print"}),
		say("ok"))
	id := r.open()
	from := r.cl.mark()
	r.prompt(id, "print it")
	for _, m := range r.cl.since(from) {
		if m.Method == "_abhed/sandbox/network_refused" {
			t.Fatalf("printed text raised the offer: %s", m.Params)
		}
	}
	old := inAgentCommand
	inAgentCommand = func() string { return "ABHED_SANDBOX is set" }
	t.Cleanup(func() { inAgentCommand = old })
	r.cl.refused(errPolicy, "_abhed/sandbox/network", map[string]any{"sessionId": id, "scope": "workspace"})
	if k := keptNetwork(r.ws); k.All {
		t.Fatal("kept from inside an agent's command")
	}
	e, _ := configEnv(config.Default(), ui.ChoiceYes)
	e.st.set = r.cl.conn.openSessions()[0].parts.Set
	if _, err := slashNetwork(context.Background(), e, []string{"workspace"}); err == nil || !strings.Contains(err.Error(), "agent's command") {
		t.Fatalf("/network inside an agent's command: %v", err)
	}
}

// Studio's request for one host with scope workspace opens and keeps that
// host alone, and the record names it.
func TestStudioNetworkWorkspaceScopeWithAHost(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	var got struct {
		Opened string `json:"opened"`
	}
	r.cl.ok("_abhed/sandbox/network", map[string]any{"sessionId": id, "scope": "workspace", "host": "example.com"}, &got)
	if !strings.Contains(got.Opened, "example.com") || strings.Contains(got.Opened, "the network to commands") {
		t.Fatalf("opened %q", got.Opened)
	}
	if k := keptNetwork(r.ws); k.All || len(k.Hosts) != 1 || k.Hosts[0] != "example.com" {
		t.Fatalf("kept %+v", k)
	}
	if changed, _ := r.recorded(id, agent.EvPermissionChanged); len(changed) != 1 || changed[0]["rule"] != "network(example.com)" {
		t.Fatalf("recorded %v", changed)
	}
	pol := r.cl.conn.session(id).parts.Set.Network
	if st := pol.State(); strings.Contains(st, "opened by the user") && !strings.Contains(st, "example.com") {
		t.Fatalf("state %q", st)
	}
}
