package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// abhed trust and the trust prompt show each workspace skill's name,
// description and hash; a grant pinned to other skills' hash trusts none.
func TestTrustShowsWorkspaceSkills(t *testing.T) {
	_, ws := trustWorkspace(t, `{}`)
	p := filepath.Join(ws, ".abhed", "skills", "release", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("---\nname: cut-release\ndescription: Cut a tagged release.\n---\nTag it.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := config.InspectWorkspace(ws)
	if err != nil || len(st.Skills) != 1 {
		t.Fatalf("%v %+v", err, st)
	}
	var out bytes.Buffer
	if code := trustCmd(ws, nil, &out); code != 0 {
		t.Fatalf("show exited %d", code)
	}
	for _, want := range []string{"skills      1 skill(s) in .abhed/skills", "cut-release", "Cut a tagged release.", st.Skills[0].SHA256[:12], st.SkillsSHA256, "NOT TRUSTED"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("show lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if _, err := askTrust(strings.NewReader("1\n"), &out, st); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cut-release", "Cut a tagged release.", st.Skills[0].SHA256[:12], "these skills?"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("the prompt lacks %q:\n%s", want, out.String())
		}
	}

	if code := trustCmd(ws, []string{"grant", "-skills-sha256", strings.Repeat("0", 64)}, io.Discard); code != 1 {
		t.Fatalf("a grant for other skills exited %d", code)
	}
	out.Reset()
	if code := trustCmd(ws, []string{"grant", "-skills-sha256", st.SkillsSHA256}, &out); code != 0 || !strings.Contains(out.String(), "Trusted 1 skill(s)") {
		t.Fatalf("grant exited %d:\n%s", code, out.String())
	}
	if st, _ := config.InspectWorkspace(ws); !st.SkillsTrusted {
		t.Fatal("the grant did not trust the skills")
	}
}
