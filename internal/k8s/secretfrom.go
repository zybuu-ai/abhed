package k8s

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/internal/kubescope"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// secret_from builds a Secret whose values never pass through the model:
// each key names a stored secret or a workspace file, Abhed reads it, and
// only the names, paths, lengths and fingerprints are judged, approved, recorded
// and shown. The dry run before the approval sends placeholders, so no value
// leaves Abhed before a person has said yes.

// maxSecretBytes is the most a Secret's values may hold in all, the API
// server's own limit.
const maxSecretBytes = 1 << 20

// secretFrom is the secret_from argument.
type secretFrom struct {
	Name string                  `json:"name"`
	Type string                  `json:"type"`
	Data map[string]secretSource `json:"data"`
}

// secretSource is where one key's value comes from: a stored secret by
// NAME, or a workspace file by absolute path.
type secretSource struct {
	Secret string `json:"secret,omitempty"`
	File   string `json:"file,omitempty"`
}

const (
	typeOpaque = "Opaque"
	typeTLS    = "kubernetes.io/tls"
)

var secretKeyRe = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)

// decodeSecretFrom reads and checks a call's secret_from, nil when it has none.
func decodeSecretFrom(raw json.RawMessage) (*secretFrom, error) {
	var a struct {
		SecretFrom *secretFrom `json:"secret_from"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("secret_from: %w", err)
	}
	sf := a.SecretFrom
	if sf == nil {
		return nil, nil
	}
	if sf.Type == "" {
		sf.Type = typeOpaque
	}
	switch {
	case sf.Name == "" || !kubescope.ValidName(sf.Name) || len(sf.Name) > 253:
		return nil, fmt.Errorf("secret_from needs a name for the Secret, one path segment")
	case sf.Type != typeOpaque && sf.Type != typeTLS:
		return nil, fmt.Errorf("secret_from type is %s or %s", typeOpaque, typeTLS)
	case len(sf.Data) == 0:
		return nil, fmt.Errorf("secret_from needs data: each key with the stored secret or workspace file it comes from")
	}
	for k, src := range sf.Data {
		if !secretKeyRe.MatchString(k) {
			return nil, fmt.Errorf("secret_from key %q is not a Secret key (letters, digits, '-', '_' and '.')", k)
		}
		switch {
		case (src.Secret == "") == (src.File == ""):
			return nil, fmt.Errorf("secret_from key %s needs exactly one of secret (a stored secret's NAME) or file (a workspace path)", k)
		case src.Secret != "" && !secrets.ValidName(src.Secret):
			return nil, fmt.Errorf("secret_from key %s: secret is the NAME of a stored secret, such as TLS_KEY, never its value", k)
		}
	}
	if sf.Type == typeTLS {
		for _, k := range []string{"tls.crt", "tls.key"} {
			if _, ok := sf.Data[k]; !ok {
				return nil, fmt.Errorf("a %s Secret needs keys tls.crt and tls.key", typeTLS)
			}
		}
	}
	return sf, nil
}

// skeleton is the Secret secret_from builds, without its values or a
// namespace: what policy, the approver and the record read as the manifest.
func (sf *secretFrom) skeleton() ([]byte, error) {
	obj, _ := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": sf.Type,
		"metadata": map[string]any{"name": sf.Name},
	})
	mf, err := kubescope.DecodeManifest(string(obj))
	if err != nil {
		return nil, err
	}
	return mf.Canonical, nil
}

func (sf *secretFrom) keys() []string {
	out := make([]string, 0, len(sf.Data))
	for k := range sf.Data {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sources names where each key comes from, never a value: once vals has
// been read, with a stored secret's length and fingerprint and a file's
// length and sha256.
func (sf *secretFrom) sources(vals ...map[string][]byte) string {
	parts := make([]string, 0, len(sf.Data))
	for _, k := range sf.keys() {
		part := k + " from " + sf.Data[k].describe()
		if len(vals) > 0 {
			if v, ok := vals[0][k]; ok {
				// A workspace file is not a secret, and its sha256 can be checked against the file.
				if sf.Data[k].Secret == "" {
					part += fmt.Sprintf(" [%d bytes, sha256 %s]", len(v), fileSum(v))
				} else {
					part += " " + withheldValue(v)
				}
			}
		}
		parts = append(parts, part)
	}
	return "builds Secret " + sf.Name + " (" + sf.Type + ") with " + strings.Join(parts, ", ")
}

// values reads each key's value: a stored secret through secret, a file
// through the session, confined to its roots.
func (sf *secretFrom) values(sess *tools.Session, secret func(string) (string, error)) (map[string][]byte, error) {
	out := map[string][]byte{}
	total := 0
	for _, k := range sf.keys() {
		src := sf.Data[k]
		var v []byte
		switch {
		case src.Secret != "":
			if secret == nil {
				return nil, fmt.Errorf("no secrets store is available here, so secret_from cannot read %s", src.Secret)
			}
			s, err := secret(src.Secret)
			if err != nil {
				return nil, fmt.Errorf("secret_from key %s: %w", k, err)
			}
			v = []byte(s)
		default:
			if sess == nil {
				return nil, fmt.Errorf("secret_from key %s: a file is read only in a session", k)
			}
			p, err := sess.Resolve(src.File)
			if err != nil {
				return nil, fmt.Errorf("secret_from key %s: %w", k, err)
			}
			if v, err = sess.ReadFile(p); err != nil {
				return nil, fmt.Errorf("secret_from key %s: cannot read %s: %w", k, src.File, err)
			}
		}
		if total += len(v); total > maxSecretBytes {
			return nil, fmt.Errorf("secret_from: the values come to more than %d bytes, a Secret's limit", maxSecretBytes)
		}
		out[k] = v
	}
	if sf.Type == typeTLS {
		if err := pemOf(out["tls.crt"], "CERTIFICATE"); err != nil {
			return nil, fmt.Errorf("secret_from tls.crt (%s): %w", sf.Data["tls.crt"].describe(), err)
		}
		if err := pemOf(out["tls.key"], "PRIVATE KEY"); err != nil {
			return nil, fmt.Errorf("secret_from tls.key (%s): %w", sf.Data["tls.key"].describe(), err)
		}
	}
	return out, nil
}

func fileSum(v []byte) string {
	sum := sha256.Sum256(v)
	return hex.EncodeToString(sum[:])
}

func (s secretSource) describe() string {
	if s.Secret != "" {
		return "stored secret " + s.Secret
	}
	return "file " + s.File
}

// pemOf checks that b holds a PEM block whose type ends in want, without
// saying anything of what it holds.
func pemOf(b []byte, want string) error {
	block, _ := pem.Decode(bytes.TrimSpace(b))
	if block == nil || !strings.HasSuffix(block.Type, want) {
		return fmt.Errorf("is not a PEM %s", want)
	}
	return nil
}

// secretRequest is the apply of a Secret built from sources: the body with
// the values, sent once approved, and a dry-run body with placeholders.
func (t ApplyTool) secretRequest(sess *tools.Session, req *request, sf *secretFrom) error {
	vals, err := sf.values(sess, t.Secret)
	if err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(req.body, &obj); err != nil {
		return err
	}
	data, placeholder, masked := map[string]any{}, map[string]any{}, map[string]any{}
	for k, v := range vals {
		data[k] = base64.StdEncoding.EncodeToString(v)
		masked[k] = withheldValue(v)
		placeholder[k] = base64.StdEncoding.EncodeToString(placeholderFor(k, sf.Type))
	}
	obj["data"] = data
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	obj["data"] = placeholder
	dry, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	req.body, req.dryBody, req.masked = body, dry, masked
	req.about = sf.sources(vals)
	return nil
}

// placeholderFor is a stand-in value the dry run sends for a key.
func placeholderFor(key, typ string) []byte {
	if typ == typeTLS && (key == "tls.crt" || key == "tls.key") {
		kind := "CERTIFICATE"
		if key == "tls.key" {
			kind = "PRIVATE KEY"
		}
		return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: []byte("abhed dry run placeholder")})
	}
	return []byte("abhed-dry-run-placeholder")
}
