package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// manyAgents is the built-ins and n loaded types with descriptions of about
// 150 bytes.
func manyAgents(n int) *Definitions {
	var defs []*Definition
	for i := range n {
		defs = append(defs, &Definition{Name: fmt.Sprintf("catalogue-specialist-%03d", i),
			Description: fmt.Sprintf("Use for questions about area %03d of the catalogue: its services, its runbooks, its alerts, its owners and the changes made to it lately.", i)})
	}
	return WithDefinitions(defs...)
}

// The task tool's listing and its agent_type enum stay within the budget
// however many types there are, and an unlisted type still runs by name.
func TestAgentListingIsBudgeted(t *testing.T) {
	d := manyAgents(100)
	task := Task{Agents: d}
	if n := len(d.listing()); n > ListingBudget {
		t.Fatalf("the agent listing is %d bytes", n)
	}
	desc := task.Description()
	if len(desc) > ListingBudget+1200 {
		t.Fatalf("the task description is %d bytes for 104 types", len(desc))
	}
	if !strings.Contains(desc, "more; give agent_type a word") {
		t.Fatalf("the listing does not say more exist: %s", desc[len(desc)-200:])
	}
	// Past the budget the enum goes: a part of the types as an enum would
	// forbid the rest, which a task may still name.
	big := manyAgents(400)
	for _, schema := range []json.RawMessage{(Task{Agents: big}).Schema(), Tasks{Agents: big}.Schema()} {
		if len(schema) > ListingBudget+2000 || strings.Contains(string(schema), "catalogue-specialist-000") {
			t.Fatalf("schema is %d bytes: %s", len(schema), schema)
		}
	}
	// Few types keep their enum.
	if s := (Task{Agents: manyAgents(3)}).Schema(); !strings.Contains(string(s), `"enum":["general"`) {
		t.Fatalf("a short list lost its enum: %s", s)
	}
}

// An unknown type names the types matching its words; an unlisted one is
// found by them.
func TestUnknownAgentTypeSearches(t *testing.T) {
	d := manyAgents(100)
	if strings.Contains(d.listing(), "catalogue-specialist-099") {
		t.Fatal("the last type is listed; the test needs it past the budget")
	}
	res := unknownType(d, "agent_type", "area 099")
	if !res.IsError || !strings.Contains(res.Content, "catalogue-specialist-099") || len(res.Content) > 4000 {
		t.Fatalf("%s", res.Content)
	}
	res = unknownType(manyAgents(400), "agent_type", "zzz")
	if !strings.Contains(res.Content, "more; give a word") || len(res.Content) > ListingBudget+300 {
		t.Fatalf("%d bytes: %s", len(res.Content), res.Content[len(res.Content)-120:])
	}
	if def, ok := d.Get("catalogue-specialist-099"); !ok || def.Name != "catalogue-specialist-099" {
		t.Fatal("an unlisted type is not found by name")
	}
}
