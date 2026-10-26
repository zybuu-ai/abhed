package monitor

import "testing"

// Credential content entered the session only where a call returned it.
func TestCredentialSeen(t *testing.T) {
	for _, c := range []struct {
		name, tool, args, out string
		want                  bool
	}{
		{"remote copy of a key", "ssh", `{"host":"web1","command":"cp -p /etc/nginx/tls/site.key /etc/nginx/tls/site.key.bak"}`, "", false},
		{"listing a key", "ssh", `{"host":"web1","command":"ls -l /etc/nginx/tls/site.key"}`, "-rw------- 1 root root 1704 site.key", false},
		{"renewing with a key", "ssh", `{"host":"web1","command":"step ca renew --force site.crt /etc/nginx/tls/site.key"}`, "Your certificate has been saved", false},
		{"checking a cert", "ssh", `{"host":"web1","command":"openssl x509 -enddate -noout -in /etc/nginx/tls/site.crt"}`, "notAfter=Jan 1 2027", false},
		{"modulus of a key", "ssh", `{"host":"web1","command":"openssl rsa -noout -modulus -in /etc/nginx/tls/site.key"}`, "Modulus=ABCDEF", false},
		{"cat a key", "ssh", `{"host":"web1","command":"cat /etc/nginx/tls/site.key"}`, "MIIEvQIBADANBg", true},
		{"base64 a key", "bash", `{"command":"base64 < ~/.ssh/id_ed25519"}`, "b3BlbnNzaC1rZXk", true},
		{"key text", "ssh", `{"host":"web1","command":"openssl rsa -in /etc/nginx/tls/site.key -text"}`, "Private-Key: (2048 bit)", true},
		{"read .env", "read", `{"path":"/ws/.env"}`, "X=1", true},
		{"read empty .env", "read", `{"path":"/ws/.env"}`, "", false},
		{"pem in output", "bash", `{"command":"kubectl get secret tls -o yaml"}`, "-----BEGIN RSA PRIVATE KEY-----", true},
		{"token in output", "bash", `{"command":"env"}`, "API_TOKEN=abcdefgh12345678", true},
		{"ordinary output", "bash", `{"command":"go test ./..."}`, "ok  pkg 0.2s", false},
		{"sed -i on a key path", "bash", `{"command":"sed -i s/a/b/ /ws/.env"}`, "x", false},
	} {
		if got := CredentialSeen(c.tool, c.args, c.out); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// Output the monitor counted as credential content before the shared list
// still counts, and only a placeholder value is let through.
func TestKeyMaterialKeepsTheEarlierShapes(t *testing.T) {
	for _, line := range []string{
		`api_key = "3f9a2b7c4d1e8f6a0b5c9d2e"`, `password = "hunter2hunter2"`, `secret_key = 'django-insecure-abc123xyz789'`,
		"db_password=Sup3rS3cretPw", "DB_PASSWORD=Tr0ub4dor&3xyz", "DB_PASSWORD=p@ss.word.2024",
		`PASSWORD="correct horse battery staple"`, "DB_PASSWORD=12345678901", "DATABASE_PASSWORD = s3cr3t-value; # comment",
		`client_secret = "9f8e7d6c5b4a39281706"`, "auth_token=Zq81kLm0Pw7xYb2", `token = "Zq81kLm0Pw7xYb2c"`,
		"API_TOKEN=abc,def,ghi,jkl", "MY_SECRET=abc:def:ghijk", "TOKEN=value1234 ./run.sh", "    token = get_token()",
	} {
		if !keyMaterial(line + "\n") {
			t.Errorf("no longer counted: %q", line)
		}
	}
	for _, line := range []string{"API_TOKEN=${API_TOKEN}", "DB_PASSWORD=changeme", `SECRET_KEY="%s"`, "export TOKEN=$TOKEN", "package main"} {
		if keyMaterial(line + "\n") {
			t.Errorf("a placeholder counted: %q", line)
		}
	}
}
