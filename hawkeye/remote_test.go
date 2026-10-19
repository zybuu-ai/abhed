package hawkeye

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Remote changes list each host's and cluster's commands in order, with the
// exit code, who let each through and the runbook step it was.
func TestRemoteChangesPerHost(t *testing.T) {
	zero, one := 0, 1
	var r rec
	r.user("rotate the certificate")
	r.add(agent.EvActionRequested, agent.ActorAgent, agent.Trusted, agent.ActionRequested{CallID: "a", Tool: "ssh",
		Args: json.RawMessage(`{"command":"nginx -t","host":"web1"}`), Target: "deploy@10.0.0.1:22"})
	r.add(agent.EvActionApproved, agent.ActorSystem, agent.Trusted, map[string]string{"call_id": "a", "step": "plan", "by": "plan",
		"plan_hash": strings.Repeat("ab", 32), "plan_step": "1"})
	r.add(agent.EvObservation, agent.ActorTool, agent.Untrusted, agent.Observation{CallID: "a", Tool: "ssh", Content: "ok", ExitCode: &zero})
	r.denied("b", "ssh", `{"command":"rm -rf /srv","host":"web1"}`, "deny", "denied by ssh(*/rm *)")
	r.add(agent.EvActionRequested, agent.ActorAgent, agent.Trusted, agent.ActionRequested{CallID: "c", Tool: "ssh",
		Args: json.RawMessage(`{"command":"systemctl reload nginx","host":"web2"}`)})
	r.add(agent.EvActionApproved, agent.ActorUser, agent.Trusted, map[string]string{"call_id": "c", "step": "default", "by": "reviewer"})
	r.add(agent.EvObservation, agent.ActorTool, agent.Untrusted, agent.Observation{CallID: "c", Tool: "ssh", Content: "failed", ExitCode: &one, IsError: true})
	r.call("d", "k8s_apply", `{"action":"restart","cluster":"lab","name":"web","namespace":"demo","resource":"deployments"}`, "default", "restarted", false)
	r.call("e", "read", `{"path":"/tmp/x"}`, "default", "x", false)
	r.end(agent.TermCompleted)

	rep := Analyze("s-test", r.evs)
	if len(rep.RemoteChanges) != 3 {
		t.Fatalf("targets %+v", rep.RemoteChanges)
	}
	web1, web2, lab := rep.RemoteChanges[0], rep.RemoteChanges[1], rep.RemoteChanges[2]
	if web1.Kind != "ssh" || web1.Name != "web1" || len(web1.Changes) != 2 || web2.Name != "web2" || lab.Kind != "k8s" || lab.Name != "lab" {
		t.Fatalf("targets %+v", rep.RemoteChanges)
	}
	a := web1.Changes[0]
	if a.What != "nginx -t" || a.Where != "deploy@10.0.0.1:22" || a.ExitCode == nil || *a.ExitCode != 0 || a.PlanStep != 1 || a.By != "plan" || len(a.PlanHash) != 64 {
		t.Fatalf("plan step %+v", a)
	}
	if b := web1.Changes[1]; b.Decision != "denied" || b.Ran {
		t.Fatalf("denied %+v", b)
	}
	if lab.Changes[0].What != "restart deployments/web" || lab.Changes[0].Namespace != "demo" {
		t.Fatalf("k8s %+v", lab.Changes[0])
	}

	text := Text(rep)
	for _, want := range []string{"Remote changes", "ssh web1", "exit 0", "[plan abababababab step 1]", "denied", "exit 1", "k8s lab", "restart deployments/web"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text lacks %q:\n%s", want, text)
		}
	}
	page, err := HTML(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Remote changes", "ssh · web1", "plan abababababab step 1", "exit 1"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %q", want)
		}
	}
}

// A hostile command is shown, not obeyed.
func TestRemoteChangesAreShownVisibly(t *testing.T) {
	var r rec
	r.call("a", "ssh", `{"command":"echo \u202eok","host":"web\u200b1"}`, "default", "", false)
	text := Text(Analyze("s-test", r.evs))
	if strings.ContainsAny(text, "\u202e\u200b") {
		t.Fatalf("hidden characters reached the text:\n%s", text)
	}
}
