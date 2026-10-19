package k8s

import (
	"encoding/base64"
	"regexp"

	"github.com/zybuu-ai/abhed/internal/secrets"
)

// lastApplied is the annotation kubectl apply keeps the whole object in, a
// Secret's values included.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// isSecret reports whether obj is a Secret.
func isSecret(obj map[string]any) bool {
	kind, _ := obj["kind"].(string)
	return kind == "Secret"
}

// maskSecret replaces each of a Secret's values with its length and fingerprint,
// keeping the keys, so no value reaches the model, the record or a screen.
// Anything under data or stringData that is not a key/value map is withheld whole.
func maskSecret(obj map[string]any) {
	for _, field := range []string{"data", "stringData"} {
		v, ok := obj[field]
		if !ok || v == nil {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			obj[field] = "[withheld: Secret " + field + "]"
			continue
		}
		for k, val := range m {
			m[k] = maskedValue(val, field == "data")
		}
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if ann, ok := meta["annotations"].(map[string]any); ok {
			if _, has := ann[lastApplied]; has {
				ann[lastApplied] = "[withheld: holds the Secret's values]"
			}
		}
	}
}

// maskedValue is what stands in for one Secret value: the decoded value's
// length and fingerprint for data, the string's own for stringData.
func maskedValue(v any, base64Encoded bool) string {
	s, ok := v.(string)
	if !ok {
		return "[withheld]"
	}
	if maskedRe.MatchString(s) {
		return s // already masked
	}
	raw := []byte(s)
	if base64Encoded {
		if b, err := base64.StdEncoding.DecodeString(s); err == nil {
			raw = b
		}
	}
	return withheldValue(raw)
}

var maskedRe = regexp.MustCompile(`^\[withheld: [0-9]+ bytes, fingerprint [0-9a-f]{12}, comparable only while this Abhed runs\]$`)

// withheldValue describes a secret value by its length and keyed
// fingerprint, never the value or a plain hash of it.
func withheldValue(raw []byte) string { return secrets.Withheld(raw) }
