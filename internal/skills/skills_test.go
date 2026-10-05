package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const validSkill = `---
name: deploy
description: Deploy the service to staging. Use when asked to deploy or ship.
---

# Deploying

1. Run the tests.
2. Push the image.
`

func TestLoadDiscoversSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "deploy", validSkill)

	reg, errs := Load([]string{root})
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	s, ok := reg.Get("deploy")
	if !ok {
		t.Fatal("skill not discovered")
	}
	if !strings.Contains(s.Description, "Use when asked") {
		t.Errorf("description = %q", s.Description)
	}
	if !strings.Contains(s.Body, "Run the tests") {
		t.Errorf("body not loaded: %q", s.Body)
	}
	if s.Dir == "" {
		t.Error("Dir not set; instructions referencing sibling files would break")
	}
}

// The listing is what sits in every request's prompt. Bodies must never be in
// it, or the whole design is pointless.
func TestListingExcludesBodies(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "deploy", validSkill)
	reg, _ := Load([]string{root})

	listing := reg.Listing()
	if !strings.Contains(listing, "deploy") || !strings.Contains(listing, "Use when asked") {
		t.Errorf("listing missing name or description: %q", listing)
	}
	if strings.Contains(listing, "Push the image") {
		t.Error("listing contains a skill body — every session would pay for it")
	}
	if !strings.Contains(listing, "skill") {
		t.Error("listing does not tell the model how to load the instructions")
	}
}

func TestEmptyRegistryRendersNothing(t *testing.T) {
	reg := NewRegistry()
	if got := reg.Listing(); got != "" {
		t.Errorf("empty registry rendered %q into every prompt", got)
	}
}

// A description is the only basis the model has for choosing a skill.
func TestParseRequiresDescription(t *testing.T) {
	_, err := Parse("---\nname: x\n---\n\nbody here\n")
	if err == nil {
		t.Fatal("accepted a skill with no description")
	}
	if !strings.Contains(err.Error(), "WHEN") {
		t.Errorf("error does not explain what a description is for: %v", err)
	}
}

func TestParseRequiresFrontmatter(t *testing.T) {
	for _, content := range []string{
		"# Just markdown\n",
		"---\nname: x\ndescription: y\n", // unterminated
	} {
		if _, err := Parse(content); err == nil {
			t.Errorf("accepted malformed skill: %q", content)
		}
	}
}

func TestParseRequiresBody(t *testing.T) {
	if _, err := Parse("---\nname: x\ndescription: y\n---\n\n\n"); err == nil {
		t.Error("accepted a skill with no instructions")
	}
}

func TestNameDefaultsToDirectory(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "my-skill", "---\ndescription: Something useful to do.\n---\n\nSteps.\n")
	reg, errs := Load([]string{root})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if _, ok := reg.Get("my-skill"); !ok {
		t.Errorf("name did not default to the directory: %v", reg.Names())
	}
}

// A later root wins, so a project can deliberately override a team skill.
func TestLaterRootOverrides(t *testing.T) {
	teamRoot, projectRoot := t.TempDir(), t.TempDir()
	writeSkill(t, teamRoot, "deploy", "---\nname: deploy\ndescription: Team version.\n---\n\nteam steps\n")
	writeSkill(t, projectRoot, "deploy", "---\nname: deploy\ndescription: Project version.\n---\n\nproject steps\n")

	reg, _ := Load([]string{teamRoot, projectRoot})
	s, _ := reg.Get("deploy")
	if !strings.Contains(s.Body, "project steps") {
		t.Errorf("later root did not win: %q", s.Body)
	}
	if reg.Len() != 1 {
		t.Errorf("override created a duplicate: %v", reg.Names())
	}
}

// A configured directory that does not exist yet is a reasonable state, not an
// error that should stop the agent starting.
func TestMissingRootIsNotAnError(t *testing.T) {
	reg, errs := Load([]string{filepath.Join(t.TempDir(), "nope")})
	if len(errs) > 0 {
		t.Errorf("a missing skills directory produced errors: %v", errs)
	}
	if reg.Len() != 0 {
		t.Error("invented skills from a missing directory")
	}
}

func TestDirectoryWithoutSkillFileIsIgnored(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "notaskill"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "notaskill", "README.md"), []byte("x"), 0o644)
	reg, errs := Load([]string{root})
	if len(errs) > 0 || reg.Len() != 0 {
		t.Errorf("a directory without SKILL.md was treated as a skill: %v %v", reg.Names(), errs)
	}
}

// ---------------------------------------------------------------- tool

func TestToolLoadsBody(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "deploy", validSkill)
	reg, _ := Load([]string{root})

	args, _ := json.Marshal(map[string]string{"name": "deploy"})
	res := Tool{R: reg}.Run(context.Background(), nil, args)
	if res.IsError {
		t.Fatalf("skill tool failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Push the image") {
		t.Errorf("body not returned: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Skill directory:") {
		t.Error("directory not reported; relative paths in instructions would be meaningless")
	}
}

// A bare "not found" sends the model into retrying variations of the name.
func TestToolUnknownSkillListsAvailable(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "deploy", validSkill)
	reg, _ := Load([]string{root})

	args, _ := json.Marshal(map[string]string{"name": "deploi"})
	res := Tool{R: reg}.Run(context.Background(), nil, args)
	if !res.IsError {
		t.Fatal("unknown skill returned success")
	}
	if !strings.Contains(res.Content, "deploy") {
		t.Errorf("refusal does not list what exists: %s", res.Content)
	}
}

// Reading instructions changes nothing; whatever they then tell the agent to
// do goes through the normal checks for those tools.
func TestToolIsReadOnly(t *testing.T) {
	if (Tool{}).Mutates() {
		t.Error("the skill tool prompts for approval, training users to click through")
	}
}

// YAML folded and literal block scalars. A real skill used `description: >`
// and parsed as the single character ">" — a description the model could never
// match a request against, so the skill was silently never invoked.
func TestParseFoldedDescription(t *testing.T) {
	s, err := Parse(`---
name: corpus-search
description: >
  Answer questions from the enterprise document corpus, with inline citations
  using the enterprise knowledge base. Use whenever the user asks a
  documented product question.
metadata:
  version: "2.0.0"
---

# Body here

Instructions.
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.Description) < 50 {
		t.Fatalf("folded description collapsed to %q", s.Description)
	}
	if !strings.Contains(s.Description, "document corpus") ||
		!strings.Contains(s.Description, "Use whenever") {
		t.Errorf("folded lines not joined: %q", s.Description)
	}
	// Folded style joins lines with spaces, not newlines.
	if strings.Contains(s.Description, "\n") {
		t.Errorf("folded description kept newlines: %q", s.Description)
	}
	// Following keys must not be swallowed into the block.
	if strings.Contains(s.Description, "version") {
		t.Errorf("block ran past its indentation: %q", s.Description)
	}
	if s.Name != "corpus-search" {
		t.Errorf("name = %q", s.Name)
	}
}

func TestParseLiteralDescription(t *testing.T) {
	s, err := Parse("---\nname: x\ndescription: |\n  Line one.\n  Line two.\n---\n\nBody.\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(s.Description, "Line one.\nLine two.") {
		t.Errorf("literal block did not keep newlines: %q", s.Description)
	}
}

// One malformed SKILL.md must not hide the rest of a directory. A stub
// alongside seven working skills silently cost all eight.
func TestOneBadSkillDoesNotBlockTheRest(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "good-one", validSkill)
	writeSkill(t, root, "stub", "---\nname: stub\ndescription: Nothing here.\n---\n")
	writeSkill(t, root, "good-two",
		"---\nname: good-two\ndescription: Another usable skill.\n---\n\nSteps.\n")

	reg, errs := Load([]string{root})
	if len(errs) != 1 {
		t.Errorf("errs = %v, want exactly the stub reported", errs)
	}
	if reg.Len() != 2 {
		t.Fatalf("loaded %v, want both working skills", reg.Names())
	}
	if !strings.Contains(errs[0].Error(), "stub") {
		t.Errorf("error does not name the offending skill: %v", errs[0])
	}
}

// Skills written for other harnesses use $SKILL_DIR. The model has no shell to
// expand it, so the tool must say what to substitute — otherwise
// "bash $SKILL_DIR/scripts/run.sh" runs /scripts/run.sh and fails.
func TestToolExplainsSkillDirSubstitution(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "deploy", validSkill)
	reg, _ := Load([]string{root})

	args, _ := json.Marshal(map[string]string{"name": "deploy"})
	res := Tool{R: reg}.Run(context.Background(), nil, args)
	if !strings.Contains(res.Content, "$SKILL_DIR") {
		t.Errorf("tool does not explain $SKILL_DIR: %s", res.Content)
	}
	if !strings.Contains(res.Content, filepath.Join(root, "deploy")) {
		t.Errorf("tool does not give the path to substitute: %s", res.Content)
	}
}

// An operator curating active skills by symlink got nothing, silently:
// os.ReadDir reports a symlink as a link, not a directory.
func TestLoadFollowsSymlinkedSkills(t *testing.T) {
	realRoot := t.TempDir()
	writeSkill(t, realRoot, "deploy", validSkill)

	activeRoot := t.TempDir()
	if err := os.Symlink(filepath.Join(realRoot, "deploy"),
		filepath.Join(activeRoot, "deploy")); err != nil {
		t.Skip("cannot create symlinks here")
	}

	reg, errs := Load([]string{activeRoot})
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if reg.Len() != 1 {
		t.Fatalf("symlinked skill not discovered: %v", reg.Names())
	}
	s, _ := reg.Get("deploy")
	if !strings.Contains(s.Body, "Push the image") {
		t.Errorf("body not read through the symlink: %q", s.Body)
	}
}

// NarrowSkills offers only the named skills, reports the ones it lacks, and
// leaves the session's own tool as it was.
func TestNarrowSkills(t *testing.T) {
	r := NewRegistry()
	r.add(&Skill{Name: "pdf", Description: "read PDFs"})
	r.add(&Skill{Name: "deploy", Description: "ship it"})
	full := Tool{R: r}
	cut, missing := full.NarrowSkills([]string{"pdf", "absent"})
	if len(missing) != 1 || missing[0] != "absent" {
		t.Fatalf("missing %v", missing)
	}
	got := cut.(Tool).R.Names()
	if len(got) != 1 || got[0] != "pdf" || full.R.Len() != 2 {
		t.Fatalf("cut %v, full %d", got, full.R.Len())
	}
	if res := cut.Run(context.Background(), nil, []byte(`{"name":"deploy"}`)); !res.IsError {
		t.Fatalf("a cut tool loaded a skill outside its list: %s", res.Content)
	}
}

// The skill tool asks to be the first call when a request matches a skill:
// models matched one and worked unaided.
func TestSkillToolAsksToBeCalledFirst(t *testing.T) {
	if d := (Tool{}).Description(); !strings.Contains(d, "call this first, before reading files or searching") {
		t.Fatalf("description: %s", d)
	}
}
