package mountns

import "testing"

func TestPlanValidate(t *testing.T) {
	ok := Plan{Root: "/w", Pin: []string{".git"}, ReadOnly: []string{".git/hooks", ".git/config"}, Empty: []string{".abhed", "sub/.abhed"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]Plan{
		"relative root": {Root: "w"},
		"escape":        {Root: "/w", ReadOnly: []string{"../etc"}},
		"absolute":      {Root: "/w", Empty: []string{"/etc"}},
		"unclean":       {Root: "/w", Pin: []string{"a/../b"}},
		"root itself":   {Root: "/w", ReadOnly: []string{"."}},
		"empty":         {Root: "/w", Empty: []string{""}},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
}
