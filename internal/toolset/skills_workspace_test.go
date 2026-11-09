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
	writeWorkspaceSkill(t, filepath.Join(ws, ".abhed", "skills"), "mine", "---\nname: mine\ndescription: The repository's take.\n---\nDo it the repo's way.\n")
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
	if !ok || !strings.Contains(s.Body, "make deploy") || !strings.HasSuffix(s.Dir, filepath.Join(".abhed", "skills", "deploy")) {
		t.Fatalf("a trusted workspace skill is not offered: %+v", s)
	}
	if s, _ := set.Skills.Get("mine"); !strings.Contains(s.Body, "my way") {
		t.Fatal("a workspace skill replaced an installed one of the same name")
	}
	if len(warned) != 1 {
		t.Fatalf("the shadowed workspace skill was not reported: %q", warned)
	}
}
