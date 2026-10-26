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

// A placeholder stands for a secret and is not one; nor is ordinary code
// that handles tokens and passwords.
func TestContentPlaceholdersAreNotSecrets(t *testing.T) {
	for _, ok := range []string{
		`dsn := fmt.Sprintf("postgres://%s:%s@%s/%s", u, p, h, d)`,
		`url = "redis://:${REDIS_PASSWORD}@cache:6379/0"`,
		"const url = `${base}?access_token=${this.accessToken}`;",
		`DATABASE_URL: postgres://postgres:postgres@db:5432/app`,
		`DATABASE_URL: postgres://app:$DB_PASSWORD@db:5432/app`,
		`dsn = "mysql://root:<password>@localhost/app"`,
		`amqp://guest:{password}@rabbit/`,
		`postgres://user:***@db/app`,
		`postgres://user:xxx@db/app`,
		`postgres://admin:changeme@db/app`,
		`https://user:@example.com/`,
		"API_TOKEN=${API_TOKEN}",
		"export DB_PASSWORD=$DB_PASSWORD",
		`SECRET_KEY = "%s"`,
		`GITHUB_TOKEN: "<your token>"`,
		"PASSWORD=changeme",
		"PASSWORD=xxxxxxxx",
		"    token = get_token()",
		"token = lexer.Next()",
		`password = request.form["password"]`,
		`refresh_token = response.json()["refresh_token"]`,
		`TOKEN_TYPE = "bearer"`,
		"PASSWORD_MIN_LENGTH = validators.MinLength(8)",
		"MAX_TOKENS = 100000000",
		"API_KEY = settings.API_KEY",
		"aws_secret_access_key = ${AWS_SECRET_ACCESS_KEY}",
		`GITHUB_TOKEN = "{{ .Token }}"`,
		"API_TOKEN=[REDACTED]",
		`SECRET_KEY = "{api_key}"`,
		"API_TOKEN=your_token_here",
		"API_TOKEN=YOUR_TOKEN_HERE",
		`export DB_PASSWORD="$(cat /run/secrets/db_password)"`,
	} {
		if out, found := RedactContent(ok); found || out != ok || HasKeyMaterial(ok) {
			t.Errorf("a placeholder or ordinary code was taken for a secret: %q -> %q", ok, out)
		}
	}
	for in, secret := range map[string]string{
		`SECRET_KEY = "a8f5f167f44f4964e6c998dee827110c"`:            "a8f5f167f44f",
		`GITHUB_TOKEN: 'Zx81kLm0Pw7xQr2'`:                            "Zx81kLm0Pw7x",
		"STRIPE_KEY=Xk9fQ2mPz8Lw4Rt7 # live":                         "Xk9fQ2mPz8Lw",
		`postgres://app:Hunter2SuperSecret@db/app`:                   "Hunter2Super",
		"DB_PASSWORD=p@ss.word.2024":                                 "p@ss.word",
		"DB_PASSWORD=Tr0ub4dor&3xyz":                                 "Tr0ub4dor",
		"API_TOKEN=abc,def,ghi,jkl":                                  "abc,def",
		"MY_SECRET=abc:def:ghijk":                                    "abc:def",
		"DATABASE_PASSWORD = s3cr3t-value; # comment":                "s3cr3t-value",
		"DB_PASSWORD=12345678901":                                    "12345678901",
		`PASSWORD="correct horse battery staple"`:                    "correct horse",
		"DB_PASSWORD=${POSTGRES_PASSWORD:-Sup3rS3cretPw}":            "Sup3rS3cretPw",
		`API_TOKEN="${API_TOKEN:-Zq81kLm0Pw7xYb2c}"`:                 "Zq81kLm0Pw7x",
		`API_TOKEN={"db":"Zq81kLm0Pw7xYb2c","s":"Ab12Cd34Ef56Gh78"}`: "Zq81kLm0Pw7x",
		`API_TOKEN=["Zq81kLm0Pw7xYb2c","Ab12Cd34Ef56Gh78"]`:          "Zq81kLm0Pw7x",
		"API_TOKEN=<Zq81kLm0Pw7xYb2c>":                               "Zq81kLm0Pw7x",
		"API_TOKEN={Zq81kLm0Pw7xYb2c}":                               "Zq81kLm0Pw7x",
		"API_TOKEN=[Zq81kLm0Pw7xYb2c]":                               "Zq81kLm0Pw7x",
		"DB_PASSWORD=YourDogIsRexTheGreat":                           "YourDogIsRex",
	} {
		if out, found := RedactContent(in); !found || strings.Contains(out, secret) {
			t.Errorf("%q -> %q", in, out)
		}
	}
}
