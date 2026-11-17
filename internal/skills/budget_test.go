package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// manySkills is a registry of n skills with descriptions of about 120 bytes.
func manySkills(t *testing.T, n int) *Registry {
	t.Helper()
	root := t.TempDir()
	for i := range n {
		writeSkill(t, root, fmt.Sprintf("catalogue-runbook-skill-%03d", i), fmt.Sprintf("---\nname: catalogue-runbook-skill-%03d\ndescription: Use when the request is about topic %03d of the catalogue, its runbooks, its alerts and its owners.\n---\n\nsteps %d\n", i, i, i))
	}
	reg, errs := Load([]string{root})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	return reg
}

// A later root's skill of the same name still wins, and says so.
func TestOverrideIsReported(t *testing.T) {
	teamRoot, projectRoot := t.TempDir(), t.TempDir()
	writeSkill(t, teamRoot, "deploy", "---\nname: deploy\ndescription: Team version.\n---\n\nteam steps\n")
	writeSkill(t, projectRoot, "deploy", "---\nname: deploy\ndescription: Project version.\n---\n\nproject steps\n")
	_, errs := Load([]string{teamRoot, projectRoot})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), `the skill "deploy" in `+projectRoot+" overrides the one in "+teamRoot) {
		t.Fatalf("errors: %v", errs)
	}
}

// The listing and the tool's names stay within the budget however many
// skills there are, and say how many more there are.
func TestListingIsBudgeted(t *testing.T) {
	reg := manySkills(t, 300)
	listing := reg.Listing()
	if len(listing) > ListingBudget+200 {
		t.Fatalf("listing is %d bytes for 300 skills", len(listing))
	}
	if !strings.Contains(listing, "more; call `skill` with a word") {
		t.Fatalf("the listing does not say more exist:\n%s", listing[len(listing)-300:])
	}
	desc := Tool{R: reg}.Description()
	if len(desc) > ListingBudget+400 || !strings.Contains(desc, "more: give a word") {
		t.Fatalf("tool description is %d bytes: %s", len(desc), desc[len(desc)-200:])
	}
}

// A skill the listing does not show loads by name, and a word finds it.
func TestUnlistedSkillLoadsAndIsFound(t *testing.T) {
	reg := manySkills(t, 300)
	if strings.Contains(reg.Listing(), "catalogue-runbook-skill-299") {
		t.Fatal("the last skill is listed; the test needs it past the budget")
	}
	run := func(name string) (string, bool) {
		args, _ := json.Marshal(map[string]string{"name": name})
		res := Tool{R: reg}.Run(context.Background(), nil, args)
		return res.Content, res.IsError
	}
	if out, isErr := run("catalogue-runbook-skill-299"); isErr || !strings.Contains(out, "steps 299") {
		t.Fatalf("by name: %v %s", isErr, out)
	}
	if out, isErr := run("topic 299"); !isErr || !strings.Contains(out, "catalogue-runbook-skill-299") {
		t.Fatalf("by a word: %v %s", isErr, out)
	}
}

// A description past 300 characters is cut, on a character, with a warning.
func TestLongDescriptionIsCapped(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "wordy", "---\nname: wordy\ndescription: "+strings.Repeat("é", 400)+"\n---\n\nbody\n")
	reg, errs := Load([]string{root})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "longer than 300 characters") {
		t.Fatalf("errors: %v", errs)
	}
	s, _ := reg.Get("wordy")
	if n := utf8.RuneCountInString(s.Description); n != MaxDescription || !utf8.ValidString(s.Description) {
		t.Fatalf("description has %d characters", n)
	}
}
