package plugin

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file verifies a Sigstore keyless signature over a manifest offline:
// no network, against a trusted root file. It checks what cosign's offline
// verification checks for a blob: the certificate chain to a trusted CA at
// the time the transparency log integrated the entry, the log's signed entry
// timestamp, that the entry covers this digest and signature, and the
// signature itself. It does not check inclusion proofs, checkpoints or the
// certificate's CT SCTs, and it reads only Rekor v1 entries, whose signed
// entry timestamp makes offline verification possible.

// Signer is who signed: the certificate's identity and its OIDC issuer.
type Signer struct {
	Identity string `json:"identity"`
	Issuer   string `json:"issuer"`
}

func (s Signer) String() string { return s.Identity + " (" + s.Issuer + ")" }

// Publisher is an allowed signer: a certificate identity and its OIDC
// issuer. A URI identity may end in '*' right after a '/' or '@', to match
// any identity it prefixes, as a workflow at any tag; an email never may.
type Publisher struct {
	Identity string `json:"identity"`
	Issuer   string `json:"issuer"`
}

// Valid says why p cannot be used, nil when it can.
func (p Publisher) Valid() error {
	if p.Identity == "" || p.Issuer == "" {
		return errors.New("a publisher names an identity and an issuer")
	}
	if u, err := url.Parse(p.Issuer); err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("issuer %q is not an https URL", p.Issuer)
	}
	for _, s := range []string{p.Identity, p.Issuer} {
		if err := checkText("publisher", s, 512); err != nil {
			return err
		}
	}
	if !normalIdentity(strings.TrimSuffix(p.Identity, "*")) {
		return fmt.Errorf("identity %q holds '..', '.', or an escaped '.' or '/'", p.Identity)
	}
	prefix, wild := strings.CutSuffix(p.Identity, "*")
	if strings.Contains(prefix, "*") {
		return fmt.Errorf("identity %q: '*' may only end it", p.Identity)
	}
	if wild {
		if !strings.Contains(prefix, "://") {
			return fmt.Errorf("identity %q: only a URI identity may end in '*'; an email is named whole", p.Identity)
		}
		if !strings.HasSuffix(prefix, "/") && !strings.HasSuffix(prefix, "@") {
			return fmt.Errorf("identity %q: '*' must follow '/' or '@', so it cannot run on into another name", p.Identity)
		}
		if u, err := url.Parse(prefix); err != nil || u.Host == "" || strings.Trim(u.Path, "/") == "" {
			return fmt.Errorf("identity %q: name at least the owner before '*'", p.Identity)
		}
	}
	return nil
}

// normalIdentity reports an identity with no dot segment and no escaped
// '.' or '/', so a prefix cannot be walked out of: https://x/acme/../evil
// and https://x/acme/%2e%2e/evil never match https://x/acme/*.
func normalIdentity(id string) bool {
	l := strings.ToLower(id)
	if strings.Contains(l, "%2e") || strings.Contains(l, "%2f") || strings.Contains(l, "%5c") || strings.Contains(l, "\\") {
		return false
	}
	if !strings.Contains(id, "://") {
		return true
	}
	_, rest, _ := strings.Cut(id, "://")
	for _, seg := range strings.Split(rest, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	u, err := url.Parse(id)
	return err == nil && (u.Path == "" || path.Clean(u.Path) == u.Path || path.Clean(u.Path)+"/" == u.Path)
}

// Matches reports whether s is this publisher. A publisher that is not
// Valid matches nobody.
func (p Publisher) Matches(s Signer) bool {
	if p.Valid() != nil || p.Issuer != s.Issuer || !normalIdentity(s.Identity) {
		return false
	}
	if prefix, ok := strings.CutSuffix(p.Identity, "*"); ok {
		return strings.HasPrefix(s.Identity, prefix)
	}
	return p.Identity == s.Identity
}

func (p Publisher) String() string { return p.Identity + " (" + p.Issuer + ")" }

// TrustedRoot is the part of a Sigstore trusted_root.json verification uses.
type TrustedRoot struct {
	cas   []trustedCA
	tlogs []trustedLog
}

type trustedCA struct {
	chain      []*x509.Certificate // leaf-most first, root last
	start, end time.Time
}

type trustedLog struct {
	keyID      []byte
	key        crypto.PublicKey
	start, end time.Time
}

type rootJSON struct {
	Tlogs []struct {
		PublicKey struct {
			RawBytes string   `json:"rawBytes"`
			ValidFor validFor `json:"validFor"`
		} `json:"publicKey"`
		LogID struct {
			KeyID string `json:"keyId"`
		} `json:"logId"`
	} `json:"tlogs"`
	CertificateAuthorities []struct {
		CertChain struct {
			Certificates []struct {
				RawBytes string `json:"rawBytes"`
			} `json:"certificates"`
		} `json:"certChain"`
		ValidFor validFor `json:"validFor"`
	} `json:"certificateAuthorities"`
}

type validFor struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

func (v validFor) times() (start, end time.Time, err error) {
	if v.Start != "" {
		if start, err = time.Parse(time.RFC3339Nano, v.Start); err != nil {
			return
		}
	}
	if v.End != "" {
		end, err = time.Parse(time.RFC3339Nano, v.End)
	}
	return
}

func within(t, start, end time.Time) bool {
	return (start.IsZero() || !t.Before(start)) && (end.IsZero() || !t.After(end))
}

//go:embed publicgood_trusted_root.json
var publicGoodRoot []byte

var publicGood = sync.OnceValue(func() *TrustedRoot {
	r, err := ParseTrustedRoot(publicGoodRoot)
	if err != nil {
		panic("plugin: the built-in public-good trusted root does not parse: " + err.Error())
	}
	return r
})

// PublicGoodRootSnapshot is when the built-in public-good root was taken.
var PublicGoodRootSnapshot = time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)

// PublicGoodRootStaleAfter is how old the built-in root may be before
// loading says so.
const PublicGoodRootStaleAfter = 365 * 24 * time.Hour

// PublicGoodRootStale reports a built-in root older than
// PublicGoodRootStaleAfter at now: Sigstore may have rotated or revoked a
// key since, and only an Abhed upgrade or a managed trusted root brings that.
func PublicGoodRootStale(now time.Time) bool {
	return now.Sub(PublicGoodRootSnapshot) > PublicGoodRootStaleAfter
}

// PublicGoodRoot is the public-good Sigstore instance's trusted root as this
// build carries it (a snapshot of sigstore/root-signing's
// targets/trusted_root.json). A bundle signed after a key rotation it does
// not know needs a newer Abhed or a trusted root given in the configuration.
func PublicGoodRoot() *TrustedRoot { return publicGood() }

// ParseTrustedRoot reads a Sigstore trusted_root.json.
func ParseTrustedRoot(data []byte) (*TrustedRoot, error) {
	var r rootJSON
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("trusted root: %w", err)
	}
	out := &TrustedRoot{}
	for i, ca := range r.CertificateAuthorities {
		var chain []*x509.Certificate
		for _, c := range ca.CertChain.Certificates {
			der, err := base64.StdEncoding.DecodeString(c.RawBytes)
			if err != nil {
				return nil, fmt.Errorf("trusted root: certificate authority %d: %w", i, err)
			}
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				return nil, fmt.Errorf("trusted root: certificate authority %d: %w", i, err)
			}
			chain = append(chain, cert)
		}
		if len(chain) == 0 {
			continue
		}
		start, end, err := ca.ValidFor.times()
		if err != nil {
			return nil, fmt.Errorf("trusted root: certificate authority %d: %w", i, err)
		}
		out.cas = append(out.cas, trustedCA{chain: chain, start: start, end: end})
	}
	for i, l := range r.Tlogs {
		der, err := base64.StdEncoding.DecodeString(l.PublicKey.RawBytes)
		if err != nil {
			return nil, fmt.Errorf("trusted root: log %d: %w", i, err)
		}
		key, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			return nil, fmt.Errorf("trusted root: log %d: %w", i, err)
		}
		id, err := base64.StdEncoding.DecodeString(l.LogID.KeyID)
		if err != nil {
			return nil, fmt.Errorf("trusted root: log %d: %w", i, err)
		}
		start, end, err := l.PublicKey.ValidFor.times()
		if err != nil {
			return nil, fmt.Errorf("trusted root: log %d: %w", i, err)
		}
		// A Rekor v1 log's id is the SHA-256 of its key, which is what binds
		// an entry to the key its SET is checked with. A log whose id is not
		// its key's (a Rekor v2 log's checkpoint key, say) is left out, so
		// an entry naming it is refused.
		if sum := sha256.Sum256(der); !bytes.Equal(sum[:], id) {
			continue
		}
		out.tlogs = append(out.tlogs, trustedLog{keyID: id, key: key, start: start, end: end})
	}
	if len(out.cas) == 0 || len(out.tlogs) == 0 {
		return nil, errors.New("trusted root: it needs at least one certificate authority and one transparency log")
	}
	return out, nil
}

type sigBundle struct {
	MediaType            string `json:"mediaType"`
	VerificationMaterial struct {
		Certificate *struct {
			RawBytes string `json:"rawBytes"`
		} `json:"certificate"`
		X509CertificateChain *struct {
			Certificates []struct {
				RawBytes string `json:"rawBytes"`
			} `json:"certificates"`
		} `json:"x509CertificateChain"`
		TlogEntries []tlogEntry `json:"tlogEntries"`
	} `json:"verificationMaterial"`
	MessageSignature *struct {
		MessageDigest *struct {
			Algorithm string `json:"algorithm"`
			Digest    string `json:"digest"`
		} `json:"messageDigest"`
		Signature string `json:"signature"`
	} `json:"messageSignature"`
}

type tlogEntry struct {
	LogIndex string `json:"logIndex"`
	LogID    struct {
		KeyID string `json:"keyId"`
	} `json:"logId"`
	KindVersion struct {
		Kind    string `json:"kind"`
		Version string `json:"version"`
	} `json:"kindVersion"`
	IntegratedTime   string `json:"integratedTime"`
	InclusionPromise *struct {
		SignedEntryTimestamp string `json:"signedEntryTimestamp"`
	} `json:"inclusionPromise"`
	CanonicalizedBody string `json:"canonicalizedBody"`
}

type hashedRekord struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Data struct {
			Hash struct {
				Algorithm string `json:"algorithm"`
				Value     string `json:"value"`
			} `json:"hash"`
		} `json:"data"`
		Signature struct {
			Content   string `json:"content"`
			PublicKey struct {
				Content string `json:"content"`
			} `json:"publicKey"`
		} `json:"signature"`
	} `json:"spec"`
}

// Fulcio's issuer extensions: .1.8 is a DER UTF8String, .1.1 the raw text.
var (
	oidIssuerV2 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
	oidIssuerV1 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
)

// skew is how far past now a log's integrated time may be.
const skew = 5 * time.Minute

// VerifySignature checks bundle, a Sigstore bundle, as a keyless signature
// over artifact against root, and returns who signed.
func VerifySignature(root *TrustedRoot, artifact, bundle []byte, now time.Time) (Signer, error) {
	if root == nil {
		return Signer{}, errors.New("no Sigstore trusted root is configured, so a signature cannot be checked")
	}
	var b sigBundle
	if err := json.Unmarshal(bundle, &b); err != nil {
		return Signer{}, fmt.Errorf("signature bundle: %w", err)
	}
	if err := checkBundleFormat(bundle, b.MediaType); err != nil {
		return Signer{}, err
	}
	var leafB64 string
	switch vm := b.VerificationMaterial; {
	case vm.Certificate != nil:
		leafB64 = vm.Certificate.RawBytes
	case vm.X509CertificateChain != nil && len(vm.X509CertificateChain.Certificates) > 0:
		leafB64 = vm.X509CertificateChain.Certificates[0].RawBytes
	default:
		return Signer{}, errors.New("signature bundle: no certificate; only keyless (certificate) signatures are accepted")
	}
	leafDER, err := base64.StdEncoding.DecodeString(leafB64)
	if err != nil {
		return Signer{}, fmt.Errorf("signature bundle: certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return Signer{}, fmt.Errorf("signature bundle: certificate: %w", err)
	}
	if b.MessageSignature == nil || b.MessageSignature.Signature == "" {
		return Signer{}, errors.New("signature bundle: no message signature")
	}
	sig, err := base64.StdEncoding.DecodeString(b.MessageSignature.Signature)
	if err != nil {
		return Signer{}, fmt.Errorf("signature bundle: signature: %w", err)
	}
	digest := sha256.Sum256(artifact)
	if md := b.MessageSignature.MessageDigest; md != nil {
		got, err := base64.StdEncoding.DecodeString(md.Digest)
		if err != nil || md.Algorithm != "SHA2_256" || !bytes.Equal(got, digest[:]) {
			return Signer{}, errors.New("signature bundle: its message digest is not this manifest's")
		}
	}

	// The log entry fixes when the signature was made: the certificate is
	// short-lived, and must have been valid then.
	if len(b.VerificationMaterial.TlogEntries) == 0 {
		return Signer{}, errors.New("signature bundle: no transparency log entry")
	}
	var integrated time.Time
	var lastErr error
	for _, e := range b.VerificationMaterial.TlogEntries {
		t, err := root.verifyEntry(e, digest[:], sig, leafDER, now)
		if err == nil {
			integrated = t
			break
		}
		lastErr = err
	}
	if integrated.IsZero() {
		return Signer{}, fmt.Errorf("signature bundle: transparency log: %w", lastErr)
	}
	if err := root.verifyChain(leaf, integrated); err != nil {
		return Signer{}, err
	}
	if err := verifyWith(leaf.PublicKey, digest[:], sig); err != nil {
		return Signer{}, fmt.Errorf("the signature does not verify over this manifest: %w", err)
	}
	return signerOf(leaf)
}

// The Sigstore bundle media types this verifier reads.
var bundleMediaTypes = []string{
	"application/vnd.dev.sigstore.bundle.v0.3+json",
	"application/vnd.dev.sigstore.bundle+json;version=0.3",
	"application/vnd.dev.sigstore.bundle+json;version=0.2",
}

// checkBundleFormat refuses, saying which, a bundle this verifier does not
// read: cosign's own legacy bundle, and Sigstore bundle versions other than
// 0.2 and 0.3.
func checkBundleFormat(raw []byte, mediaType string) error {
	if mediaType == "" {
		var legacy map[string]json.RawMessage
		if json.Unmarshal(raw, &legacy) == nil {
			if _, ok := legacy["base64Signature"]; ok {
				return errors.New("signature bundle: this is cosign's legacy bundle format, which is not supported; " +
					"sign with cosign 3 (cosign sign-blob --bundle FILE.sigstore.json), or cosign 2.4 or later with --new-bundle-format")
			}
		}
		return errors.New("signature bundle: it has no mediaType, so it is not a Sigstore bundle")
	}
	for _, m := range bundleMediaTypes {
		if strings.EqualFold(strings.ReplaceAll(mediaType, " ", ""), m) {
			return nil
		}
	}
	return fmt.Errorf("signature bundle: media type %q is not supported; Sigstore bundles 0.2 and 0.3 are", mediaType)
}

func (r *TrustedRoot) verifyEntry(e tlogEntry, digest, sig, leafDER []byte, now time.Time) (time.Time, error) {
	if e.KindVersion.Kind != "hashedrekord" {
		return time.Time{}, fmt.Errorf("entry kind %q is not hashedrekord", e.KindVersion.Kind)
	}
	if e.KindVersion.Version != "0.0.1" || e.InclusionPromise == nil || e.InclusionPromise.SignedEntryTimestamp == "" {
		return time.Time{}, errors.New("the entry carries no signed entry timestamp (as a Rekor v2 log's do not, relying on an RFC 3161 timestamp and a checkpoint), " +
			"and only Rekor v1 entries with one are verified offline here")
	}
	keyID, err := base64.StdEncoding.DecodeString(e.LogID.KeyID)
	if err != nil {
		return time.Time{}, err
	}
	secs, err := strconv.ParseInt(e.IntegratedTime, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("integrated time: %w", err)
	}
	idx, err := strconv.ParseInt(e.LogIndex, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("log index: %w", err)
	}
	at := time.Unix(secs, 0)
	if at.After(now.Add(skew)) {
		return time.Time{}, errors.New("the entry's integrated time is in the future")
	}
	var log *trustedLog
	for i := range r.tlogs {
		if bytes.Equal(r.tlogs[i].keyID, keyID) {
			log = &r.tlogs[i]
			break
		}
	}
	if log == nil {
		return time.Time{}, errors.New("the entry is from a log the trusted root does not name")
	}
	if !within(at, log.start, log.end) {
		return time.Time{}, errors.New("the log's key was not valid when the entry was integrated")
	}
	set, err := base64.StdEncoding.DecodeString(e.InclusionPromise.SignedEntryTimestamp)
	if err != nil {
		return time.Time{}, err
	}
	// Rekor signs the canonical JSON of these four fields, keys in order.
	payload, err := json.Marshal(struct {
		Body           string `json:"body"`
		IntegratedTime int64  `json:"integratedTime"`
		LogID          string `json:"logID"`
		LogIndex       int64  `json:"logIndex"`
	}{e.CanonicalizedBody, secs, hex.EncodeToString(keyID), idx})
	if err != nil {
		return time.Time{}, err
	}
	sum := sha256.Sum256(payload)
	if err := verifyWith(log.key, sum[:], set); err != nil {
		return time.Time{}, fmt.Errorf("the signed entry timestamp does not verify: %w", err)
	}
	bodyJSON, err := base64.StdEncoding.DecodeString(e.CanonicalizedBody)
	if err != nil {
		return time.Time{}, err
	}
	var body hashedRekord
	if err := json.Unmarshal(bodyJSON, &body); err != nil {
		return time.Time{}, fmt.Errorf("entry body: %w", err)
	}
	// The SET covers the body, not kindVersion: the body says what it is.
	if body.Kind != "hashedrekord" || body.APIVersion != "0.0.1" {
		return time.Time{}, fmt.Errorf("the log entry is a %s %s, not a hashedrekord 0.0.1", body.Kind, body.APIVersion)
	}
	if body.Spec.Data.Hash.Algorithm != "sha256" || body.Spec.Data.Hash.Value != hex.EncodeToString(digest) {
		return time.Time{}, errors.New("the log entry is for another artifact")
	}
	if got, err := base64.StdEncoding.DecodeString(body.Spec.Signature.Content); err != nil || !bytes.Equal(got, sig) {
		return time.Time{}, errors.New("the log entry is for another signature")
	}
	pemBytes, err := base64.StdEncoding.DecodeString(body.Spec.Signature.PublicKey.Content)
	if err != nil {
		return time.Time{}, err
	}
	blk, _ := pem.Decode(pemBytes)
	if blk == nil || !bytes.Equal(blk.Bytes, leafDER) {
		return time.Time{}, errors.New("the log entry is for another certificate")
	}
	return at, nil
}

func (r *TrustedRoot) verifyChain(leaf *x509.Certificate, at time.Time) error {
	if !within(at, leaf.NotBefore, leaf.NotAfter) {
		return errors.New("the signing certificate was not valid when the log integrated the entry")
	}
	var lastErr error
	for _, ca := range r.cas {
		if !within(at, ca.start, ca.end) {
			lastErr = errors.New("no certificate authority in the trusted root was valid at signing time")
			continue
		}
		roots, inter := x509.NewCertPool(), x509.NewCertPool()
		roots.AddCert(ca.chain[len(ca.chain)-1])
		for _, c := range ca.chain[:len(ca.chain)-1] {
			inter.AddCert(c)
		}
		_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: at,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}})
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("the signing certificate does not chain to the trusted root: %w", lastErr)
}

func verifyWith(key crypto.PublicKey, digest, sig []byte) error {
	switch k := key.(type) {
	case *ecdsa.PublicKey:
		if ecdsa.VerifyASN1(k, digest, sig) {
			return nil
		}
		return errors.New("bad ECDSA signature")
	}
	// Fulcio and Rekor sign with ECDSA; anything else is not accepted here.
	return fmt.Errorf("unsupported key type %T", key)
}

func signerOf(c *x509.Certificate) (Signer, error) {
	var s Signer
	switch {
	case len(c.EmailAddresses)+len(c.URIs) > 1:
		return s, errors.New("the signing certificate names more than one identity")
	case len(c.EmailAddresses) == 1:
		s.Identity = c.EmailAddresses[0]
	case len(c.URIs) == 1:
		s.Identity = c.URIs[0].String()
	default:
		return s, errors.New("the signing certificate names no identity")
	}
	var v1, v2 string
	var hasV1, hasV2 bool
	for _, ext := range c.Extensions {
		switch {
		case ext.Id.Equal(oidIssuerV2):
			if hasV2 {
				return s, errors.New("the signing certificate names its issuer twice")
			}
			hasV2 = true
			rest, err := asn1.UnmarshalWithParams(ext.Value, &v2, "utf8")
			if err != nil || len(rest) > 0 || v2 == "" {
				return s, errors.New("the signing certificate's issuer extension (1.3.6.1.4.1.57264.1.8) does not read")
			}
		case ext.Id.Equal(oidIssuerV1):
			if hasV1 {
				return s, errors.New("the signing certificate names its issuer twice")
			}
			hasV1, v1 = true, string(ext.Value)
		}
	}
	switch {
	case hasV1 && hasV2 && v1 != v2:
		return s, fmt.Errorf("the signing certificate names two issuers, %q and %q", v2, v1)
	case hasV2:
		s.Issuer = v2
	case hasV1:
		s.Issuer = v1
	}
	if s.Issuer == "" {
		return s, errors.New("the signing certificate names no OIDC issuer")
	}
	return s, nil
}
