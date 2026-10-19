package hawkeye

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func TestSupervisorSection(t *testing.T) {
	r := (&rec{}).user("update the README")
	r.denied("c1", "bash", `{"command":"rm -rf /srv/data"}`, "monitor", "supervisor: out-of-remit — rm would destroy /srv/data")
	r.add(agent.EvMonitorVerdict, agent.ActorSystem, agent.Trusted, agent.MonitorVerdict{CallID: "c1", Before: "allow", After: "deny",
		Code: "out-of-remit", Rationale: "rm would destroy /srv/data\x1b[2J", LatencyMS: 1, Version: "rules-v1"})
	r.add(agent.EvSupervisorCorrection, agent.ActorSystem, agent.Trusted, agent.SupervisorCorrection{CallID: "c1", Code: "out-of-remit", Text: "Supervisor: out-of-remit."})
	r.call("c2", "bash", `{"command":"go test ./..."}`, "monitor", "ok", false)
	r.add(agent.EvMonitorVerdict, agent.ActorSystem, agent.Trusted, agent.MonitorVerdict{CallID: "c2", Before: "allow", After: "allow", Code: "ok", Version: "rules-v1"})
	r.end(agent.TermCompleted)

	rep := Analyze("s-test", r.evs)
	s := rep.Supervisor
	if s == nil || s.Reviewed != 2 || s.Tightened != 1 || s.Corrections != 1 || s.ByCode["out-of-remit"] != 1 || len(s.Concerns) != 1 ||
		s.Concerns[0].Tool != "bash" || len(s.Judges) != 1 {
		t.Fatalf("%+v", s)
	}
	text := Text(rep)
	if !strings.Contains(text, "supervisor 2 reviewed · 1 tightened") || !strings.Contains(text, "out-of-remit 1") || strings.Contains(text, "\x1b[2J") {
		t.Fatalf("text:\n%s", text)
	}
	page, err := HTML(rep)
	if err != nil || !strings.Contains(page, "<h2>Supervisor</h2>") {
		t.Fatalf("html: %v", err)
	}
	if rep := Analyze("s-test", (&rec{}).user("x").end(agent.TermCompleted).evs); rep.Supervisor != nil {
		t.Fatal("a record without the supervisor has a supervisor section")
	}
}
