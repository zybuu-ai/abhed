package secretfiles

import (
	"strings"
	"testing"
)

func TestRedactContent(t *testing.T) {
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEA\n-----END OPENSSH PRIVATE KEY-----"
	for _, in := range []string{
		"x := `" + key + "`", "aws_key = \"AKIAIOSFODNN7EXAMPLE\"", "token: ghp_" + strings.Repeat("a", 36),
		"OPENAI=sk-proj-" + strings.Repeat("b", 40), "-----BEGIN RSA PRIVATE KEY-----\nMIIE (cut off",
	} {
		out, found := RedactContent(in)
		if !found || !strings.Contains(out, "[secret]") || strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") || strings.Contains(out, "b3BlbnNz") || strings.Contains(out, "MIIE") {
			t.Errorf("%q -> %q %v", in, out, found)
		}
	}
	if out, found := RedactContent("func add(a, b int) int { return a + b }"); found || out != "func add(a, b int) int { return a + b }" {
		t.Errorf("ordinary code changed: %q", out)
	}
}

// Each shape is found, and its secret part replaced, by the one list.
func TestContentPatterns(t *testing.T) {
	for name, tc := range map[string]struct{ in, secret string }{
		"jwt":          {`auth := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"`, "SflKxwRJSMeKKF2QT4fw"},
		"slack hook":   {`url := "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"`, "XXXXXXXXXXXXXXXXXXXXXXXX"},
		"stripe live":  {`key := "sk_live_` + strings.Repeat("a1B2", 6) + `"`, "a1B2a1B2a1B2"},
		"stripe rk":    {`key := "rk_live_` + strings.Repeat("a1B2", 6) + `"`, "a1B2a1B2a1B2"},
		"anthropic":    {`key := "sk-ant-api03-` + strings.Repeat("Ab3_", 20) + `"`, "Ab3_Ab3_Ab3_"},
		"openai short": {`key := "sk-` + strings.Repeat("Ab3x", 6) + `"`, "Ab3xAb3xAb3x"},
		"url password": {`dsn := "postgres://admin:Hunter2SuperSecret@db.internal:5432/app"`, "Hunter2SuperSecret"},
		"url query":    {`u := "https://api.example.com/v1?access_token=Zq81kLm0Pw7x"`, "Zq81kLm0Pw7x"},
		"env secret":   {"DB_PASSWORD=Xk9fQ2mPz8Lw4Rt7", "Xk9fQ2mPz8Lw4Rt7"},
		"env token":    {"export API_TOKEN=Xk9fQ2mPz8Lw4Rt7Yb1", "Xk9fQ2mPz8Lw4Rt7Yb1"},
		"aws secret":   {"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "wJalrXUtnFEMI"},
		"github pat40": {`t := "github_pat_` + strings.Repeat("A1b2", 11) + `"`, "A1b2A1b2A1b2"},
		"pem ec":       {"-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\n-----END EC PRIVATE KEY-----", "MHcCAQEE"},
	} {
		out, found := RedactContent(tc.in)
		if !found || !HasKeyMaterial(tc.in) || strings.Contains(out, tc.secret) || !strings.Contains(out, "[secret]") {
			t.Errorf("%s: %q -> %q (found %v)", name, tc.in, out, found)
		}
	}
	for _, ok := range []string{
		"func add(a, b int) int { return a + b }",
		`resp, err := http.Get("https://example.com/path?page=2")`,
		"x := token.Next()",
	} {
		if out, found := RedactContent(ok); found || out != ok || HasKeyMaterial(ok) {
			t.Errorf("ordinary code changed: %q -> %q", ok, out)
		}
	}
}
