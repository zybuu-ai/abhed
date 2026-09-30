// Package secretfiles names the files and folders that hold keys and
// credentials, so every part of Abhed that treats them specially agrees.
package secretfiles

import (
	"path/filepath"
	"strings"
)

// Names are glob patterns matched, in lower case, against each part of a
// path: a file of keys, or a folder that holds them.
var Names = []string{
	".env", ".env.*", "*.env", ".envrc", "*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore", "*.kdbx",
	"id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*", ".netrc", ".npmrc", ".pypirc", ".pgpass", ".htpasswd",
	".git-credentials", "credentials", "credentials.json", "*.tfvars", "*.tfstate", ".vault-token", ".s3cfg", ".boto",
	".ssh", ".aws", ".azure", "gcloud", ".gnupg", ".kube", ".docker",
}

// Match returns the pattern a part of path matches, "" when none does. Case
// is ignored, as a case-insensitive disk opens .ENV as .env.
func Match(path string) string {
	for _, part := range strings.FieldsFunc(strings.ToLower(path), func(r rune) bool { return r == '/' || r == '\\' }) {
		for _, pat := range Names {
			if ok, _ := filepath.Match(pat, part); ok {
				return pat
			}
		}
	}
	return ""
}

// InText returns the pattern a path-like word of text matches, such as a
// file named in a command, "" when none does.
func InText(text string) string {
	for _, word := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || strings.ContainsRune("\"'=;|&<>(),`", r)
	}) {
		if m := Match(word); m != "" {
			return m
		}
	}
	return ""
}
