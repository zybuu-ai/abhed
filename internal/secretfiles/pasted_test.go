package secretfiles

import (
	"strings"
	"testing"
)

func TestFindPastedLoginCommand(t *testing.T) {
	tok := "sha256~Zq1cP3xYv8mN0aB4dE6fG7hI9jK2lM5nO8pQrStUvWx"
	for _, msg := range []string{
		"please log in: oc login --token=" + tok + " --server=https://api.lab.example:6443 -n web",
		"oc login --server=https://api.lab.example:6443 --token " + tok + " --namespace=web",
		"oc login https://api.lab.example:6443 --token=" + tok + " -n web",
	} {
		got := FindPasted(msg)
		if len(got) != 1 {
			t.Fatalf("%q: found %d", msg, len(got))
		}
		p := got[0]
		if p.Value(msg) != tok || p.Kind != "an OpenShift token" {
			t.Errorf("%q: found %q as %s", msg, p.Value(msg), p.Kind)
		}
		if p.Server != "https://api.lab.example:6443" || p.Namespace != "web" {
			t.Errorf("%q: server %q namespace %q", msg, p.Server, p.Namespace)
		}
	}
}

func TestFindPastedShapes(t *testing.T) {
	for _, tc := range []struct{ msg, value, kind string }{
		{"the token is sha256~Zq1cP3xYv8mN0aB4dE6fG7hI9jK2lM5nO8pQr, use it", "sha256~Zq1cP3xYv8mN0aB4dE6fG7hI9jK2lM5nO8pQr", "an OpenShift token"},
		{"curl -H 'Authorization: Bearer x9Fq2LmZp4Rt7Wv1Yb3Nc8Kd' https://h", "x9Fq2LmZp4Rt7Wv1Yb3Nc8Kd", "a bearer token"},
		{"my key ghp_aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3zA5bC", "ghp_aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3zA5bC", "a GitHub token"},
		{"AKIAIOSFODNN7EXAMPLF is the id", "AKIAIOSFODNN7EXAMPLF", "an AWS access key id"},
	} {
		got := FindPasted(tc.msg)
		if len(got) != 1 || got[0].Value(tc.msg) != tc.value || got[0].Kind != tc.kind {
			t.Errorf("%q: found %+v", tc.msg, got)
		}
	}
}

// A placeholder, an expansion or prose about tokens is not a credential.
func TestFindPastedLeavesPlaceholders(t *testing.T) {
	for _, msg := range []string{
		"run oc login --token=<token> --server=https://api.x:6443",
		"run oc login --token=$TOKEN --server=https://api.x:6443",
		"how do bearer tokens work in kubernetes?",
		"what does sha256~ mean in an OpenShift token",
	} {
		if got := FindPasted(msg); len(got) != 0 {
			t.Errorf("%q: found %+v", msg, got)
		}
	}
}

// The kinds name the patterns they are said to: a pattern moved in the
// list must not be named as another.
func TestPastedKindsMatchTheirPatterns(t *testing.T) {
	want := map[int]string{0: "PRIVATE KEY", 1: "AKIA", 2: "gh[pousr]_", 4: "glpat-", 5: "xox", 8: "AIza", 9: "eyJ", 14: "Bearer"}
	for i, frag := range want {
		if !strings.Contains(contentPatterns[i].re.String(), frag) {
			t.Errorf("pattern %d is %s, not the one kinds names %q", i, contentPatterns[i].re, kinds[i])
		}
	}
}

func TestFindPastedLoginInBackticksAcrossLinesAndSetCredentials(t *testing.T) {
	const tok = "q8Zt3kLm9PxR2vWn7YbC4dFg"
	for _, tc := range []struct{ msg, server string }{
		{"run `oc login --token=" + tok + " --server=https://api.x.example:6443`", "https://api.x.example:6443"},
		{"oc login \\\n  --server=https://api.x.example:6443 \\\n  --token=" + tok, "https://api.x.example:6443"},
		{"kubectl config set-credentials me --token=" + tok, ""},
	} {
		got := FindPasted(tc.msg)
		if len(got) != 1 || got[0].Value(tc.msg) != tok || got[0].Server != tc.server {
			t.Errorf("%q: found %+v", tc.msg, got)
		}
	}
}
