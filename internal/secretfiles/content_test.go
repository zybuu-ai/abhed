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
