// Package orgpolicy is the signed organisation policy: a managed
// configuration document an organisation's server signs with an Ed25519 key
// the client pinned, cached under Abhed's state and applied as the managed
// layer below the machine's own managed file.
//
// The wire and cache form is a DSSE envelope whose payload is a Statement.
// Anything that can serve such an envelope over HTTPS is a source; the
// Enterprise server is one.
//
// It is an experimental API until 2.0, and may change in a minor release.
package orgpolicy

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PayloadType is the DSSE payload type of a signed policy statement.
const PayloadType = "application/vnd.abhed.org-policy+json"

// StatementVersion is the statement format this build reads and writes.
const StatementVersion = 1

// DefaultMaxAge is how long a verified policy is applied without a fresh one
// when neither the statement nor the machine's managed file says.
const DefaultMaxAge = 72 * time.Hour

// Skew is how far in the future a statement's issued_at may be.
const Skew = 5 * time.Minute

// fallback is the strictest policy: applied over what was cached when no
// verified, fresh policy can be had. Plan mode, no allow rules, no web tools.
const fallback = `{"permissions":{"mode":"plan","allow":[]},"web_search":{"enabled":false},"web_fetch":{"enabled":false}}`

// Fallback is a copy of the fallback policy document.
func Fallback() []byte { return []byte(fallback) }

// Statement is what the organisation signs.
type Statement struct {
	V int `json:"v"`
	// Tenant and Subject are who the server issued it for.
	Tenant  string `json:"tenant"`
	Subject string `json:"subject,omitempty"`
	// Name is the policy chosen (a group's, or "default"), Version its
	// published version, and Epoch the server's publish count, which never
	// goes down: a client refuses a statement from an earlier epoch.
	Name    string `json:"name"`
	Version int64  `json:"version"`
	Epoch   int64  `json:"epoch"`
	// IssuedAt is when it was signed; it is applied for at most MaxAgeSeconds after.
	IssuedAt      time.Time `json:"issued_at"`
	MaxAgeSeconds int64     `json:"max_age_seconds,omitempty"`
	// Keys are the signing keys the organisation uses now and next, base64
	// Ed25519; a verified statement replaces the client's pins with them.
	Keys []string `json:"keys,omitempty"`
	// Policy is the managed configuration document, a JSON object.
	Policy json.RawMessage `json:"policy"`
}

// MaxAge is the statement's own limit, or DefaultMaxAge.
func (s Statement) MaxAge() time.Duration {
	if s.MaxAgeSeconds > 0 {
		return time.Duration(s.MaxAgeSeconds) * time.Second
	}
	return DefaultMaxAge
}

// Digest is the sha256 of the policy document as signed.
func (s Statement) Digest() string {
	sum := sha256.Sum256(s.Policy)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Envelope is a DSSE envelope.
type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

// Signature is one signer's; KeyID is KeyID of its public key.
type Signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// PAE is DSSE's pre-authentication encoding, what is signed.
func PAE(payloadType string, payload []byte) []byte {
	b := []byte("DSSEv1 " + strconv.Itoa(len(payloadType)) + " " + payloadType + " " + strconv.Itoa(len(payload)) + " ")
	return append(b, payload...)
}

// KeyID is sha256:<hex> of the key's PKIX DER, as the Enterprise key
// service names keys.
func KeyID(pub ed25519.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// EncodeKey is a public key as statements and pins carry it.
func EncodeKey(pub ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(pub) }

// ParseKey reads a public key: base64 Ed25519, or a PEM PUBLIC KEY.
func ParseKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if blk, _ := pem.Decode([]byte(s)); blk != nil {
		if blk.Type != "PUBLIC KEY" {
			return nil, errors.New("the PEM block is not a PUBLIC KEY")
		}
		pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		k, ok := pub.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("the key is not Ed25519")
		}
		return k, nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("not a base64 Ed25519 public key or a PEM PUBLIC KEY")
	}
	return ed25519.PublicKey(b), nil
}

// ParseKeys reads several keys, refusing any that does not parse.
func ParseKeys(in []string) ([]ed25519.PublicKey, error) {
	out := make([]ed25519.PublicKey, 0, len(in))
	for i, s := range in {
		k, err := ParseKey(s)
		if err != nil {
			return nil, fmt.Errorf("key %d: %w", i+1, err)
		}
		out = append(out, k)
	}
	return out, nil
}

// Sign signs s with priv. A server with its own key service signs PAE itself.
func Sign(s Statement, priv ed25519.PrivateKey) (Envelope, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return Envelope{}, err
	}
	pub, _ := priv.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(priv, PAE(PayloadType, payload))
	return Envelope{PayloadType: PayloadType, Payload: base64.StdEncoding.EncodeToString(payload),
		Signatures: []Signature{{KeyID: KeyID(pub), Sig: base64.StdEncoding.EncodeToString(sig)}}}, nil
}

// ErrUntrusted is an envelope no pinned key signed.
var ErrUntrusted = errors.New("no signature by a pinned key holds")

// Open checks e is a policy statement signed by one of keys and returns it
// with the id of the key that signed. Signatures by other keys are ignored.
func Open(e Envelope, keys []ed25519.PublicKey) (Statement, string, error) {
	var st Statement
	if e.PayloadType != PayloadType {
		return st, "", fmt.Errorf("the envelope holds %q, not a policy", e.PayloadType)
	}
	if len(keys) == 0 {
		return st, "", errors.New("no signing key is pinned")
	}
	payload, err := base64.StdEncoding.DecodeString(e.Payload)
	if err != nil {
		return st, "", errors.New("the envelope's payload is not base64")
	}
	msg := PAE(e.PayloadType, payload)
	signer := ""
	for _, s := range e.Signatures {
		sig, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil {
			continue
		}
		for _, k := range keys {
			if KeyID(k) == s.KeyID && ed25519.Verify(k, msg, sig) {
				signer = s.KeyID
				break
			}
		}
		if signer != "" {
			break
		}
	}
	if signer == "" {
		return st, "", ErrUntrusted
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return st, "", fmt.Errorf("the statement does not read: %w", err)
	}
	if dec.More() {
		return st, "", errors.New("the statement holds more than one value")
	}
	if err := st.check(); err != nil {
		return st, "", err
	}
	return st, signer, nil
}

func (s Statement) check() error {
	switch {
	case s.V != StatementVersion:
		return fmt.Errorf("statement version %d is not one this build reads", s.V)
	case s.IssuedAt.IsZero():
		return errors.New("the statement has no issued_at")
	case s.Epoch < 0 || s.Version < 0 || s.MaxAgeSeconds < 0:
		return errors.New("the statement has a negative epoch, version or max age")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(s.Policy, &obj); err != nil || obj == nil {
		return errors.New("the policy is not a JSON object")
	}
	if _, err := ParseKeys(s.Keys); err != nil {
		return fmt.Errorf("the statement's keys: %w", err)
	}
	return nil
}
