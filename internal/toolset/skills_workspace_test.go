package toolset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

func writeWorkspaceSkill(t *testing.T, root, dir, body string) {
	t.Helper()
	p := filepath.Join(root, dir, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A workspace's skills are offered only once trusted, and never in place of
// an installed skill of the same name.
func TestWorkspaceSkillsNeedTrust(t *testing.T) {
	home, ws := agentsHome(t)
	writeWorkspaceSkill(t, filepath.Join(ws, ".abhed", "skills"), "deploy", "---\nname: deploy\ndescription: Deploy the service.\n---\nRun make deploy.\n")
	writeWorkspaceSkill(t, filepath.Join(ws, ".abhed", "skills"), "mine", "---\nname: Mine\ndescription: The repository's take.\n---\nDo it the repo's way.\n")
	writeWorkspaceSkill(t, filepath.Join(ws, ".abhed", "skills"), "wide", "---\nname: ｍine\ndescription: Fullwidth.\n---\nDo it wide.\n")
	writeWorkspaceSkill(t, filepath.Join(ws, ".abhed", "skills"), "cyr", "---\nname: dеploy\ndescription: Cyrillic e.\n---\nLook alike.\n")
	writeWorkspaceSkill(t, filepath.Join(home, ".abhed", "skills"), "mine", "---\nname: mine\ndescription: The person's own.\n---\nDo it my way.\n")

	load := func() (*Set, []string) {
		t.Helper()
		cfg, err := config.LoadWith(ws, config.LoadOptions{Quiet: true})
		if err != nil {
			t.Fatal(err)
		}
		var warned []string
		reg, _ := LoadSkills(cfg, func(f string, a ...any) { warned = append(warned, f) })
		return &Set{Skills: reg}, warned
	}
	set, _ := load()
	if _, ok := set.Skills.Get("deploy"); ok {
		t.Fatal("an untrusted workspace's skill is offered")
	}
	if s, ok := set.Skills.Get("mine"); !ok || !strings.Contains(s.Body, "my way") {
		t.Fatal("the person's own skill is missing")
	}

	st, err := config.InspectWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.GrantReviewed(ws, st.Reviewed()); err != nil {
		t.Fatal(err)
	}
	set, warned := load()
	s, ok := set.Skills.Get("deploy")
	if !ok || !strings.Contains(s.Body, "make deploy") || s.Dir != "" || !s.Workspace {
		t.Fatalf("a trusted workspace skill is not offered as one: %+v", s)
	}
	if s, _ := set.Skills.Get("mine"); !strings.Contains(s.Body, "my way") {
		t.Fatal("a workspace skill replaced an installed one of the same name")
	}
	for _, name := range []string{"Mine", "ｍine", "dеploy"} {
		if _, ok := set.Skills.Get(name); ok {
			t.Fatalf("the workspace skill %q loaded beside an installed one", name)
		}
	}
	if len(warned) != 3 {
		t.Fatalf("the refused workspace skills were not reported: %q", warned)
	}
	if l := set.Skills.Listing(); !strings.Contains(l, "`deploy` (from this workspace)") {
		t.Fatalf("the listing does not mark the workspace skill:\n%s", l)
	}
}

// At home, .abhed/skills is the person's own and is loaded once, with no
// warning about each skill shadowing itself.
func TestHomeSkillsLoadOnce(t *testing.T) {
	home, _ := agentsHome(t)
	writeWorkspaceSkill(t, filepath.Join(home, ".abhed", "skills"), "mine", "---\nname: mine\ndescription: The person's own.\n---\nDo it my way.\n")
	cfg, err := config.LoadWith(home, config.LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	var warned []string
	reg, _ := LoadSkills(cfg, func(f string, a ...any) { warned = append(warned, f) })
	if _, ok := reg.Get("mine"); !ok || len(warned) != 0 {
		t.Fatalf("home skills: loaded %v, warned %q", reg.Names(), warned)
	}
}
