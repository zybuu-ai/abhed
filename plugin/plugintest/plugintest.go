// Package plugintest builds plugin bundles and a stand-in Sigstore for
// tests: a certificate authority, a transparency log key, and keyless-style
// signatures over a manifest, all made in memory. Nothing reaches a network.
package plugintest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/plugin"
)

// Sigstore is a certificate authority and a log, as a trusted root names them.
type Sigstore struct {
	caKey   *ecdsa.PrivateKey
	caCert  *x509.Certificate
	caDER   []byte
	logKey  *ecdsa.PrivateKey
	logDER  []byte
	logID   []byte
	nextIdx int64
	// Now is the signing time; zero is time.Now.
	Now time.Time
}

// NewSigstore makes a fresh authority and log.
func NewSigstore() *Sigstore {
	s := &Sigstore{}
	s.caKey = must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-fulcio"},
		NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	s.caDER = must(x509.CreateCertificate(rand.Reader, tmpl, tmpl, &s.caKey.PublicKey, s.caKey))
	s.caCert = must(x509.ParseCertificate(s.caDER))
	s.logKey = must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	s.logDER = must(x509.MarshalPKIXPublicKey(&s.logKey.PublicKey))
	sum := sha256.Sum256(s.logDER)
	s.logID = sum[:]
	return s
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func (s *Sigstore) now() time.Time {
	if s.Now.IsZero() {
		return time.Now()
	}
	return s.Now
}

// TrustedRoot is the trusted_root.json naming this authority and log.
func (s *Sigstore) TrustedRoot() []byte {
	b64 := base64.StdEncoding.EncodeToString
	root := map[string]any{
		"mediaType": "application/vnd.dev.sigstore.trustedroot+json;version=0.1",
		"tlogs": []any{map[string]any{
			"baseUrl": "https://rekor.test", "hashAlgorithm": "SHA2_256",
			"publicKey": map[string]any{"rawBytes": b64(s.logDER), "keyDetails": "PKIX_ECDSA_P256_SHA_256",
				"validFor": map[string]any{"start": "2020-01-01T00:00:00Z"}},
			"logId": map[string]any{"keyId": b64(s.logID)},
		}},
		"certificateAuthorities": []any{map[string]any{
			"uri":       "https://fulcio.test",
			"certChain": map[string]any{"certificates": []any{map[string]any{"rawBytes": b64(s.caDER)}}},
			"validFor":  map[string]any{"start": "2020-01-01T00:00:00Z"},
		}},
	}
	return must(json.Marshal(root))
}

// Sign makes a Sigstore bundle over artifact for a signer with this
// identity (an email, or a URI when it has "://") and OIDC issuer.
func (s *Sigstore) Sign(artifact []byte, identity, issuer string) []byte {
	at := s.now()
	key := must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	issuerExt := must(asn1.MarshalWithParams(issuer, "utf8"))
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(at.UnixNano()),
		NotBefore:    at.Add(-time.Minute), NotAfter: at.Add(10 * time.Minute),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}, Value: issuerExt}},
	}
	if strings.Contains(identity, "://") {
		tmpl.URIs = []*url.URL{must(url.Parse(identity))}
	} else {
		tmpl.EmailAddresses = []string{identity}
	}
	leafDER := must(x509.CreateCertificate(rand.Reader, tmpl, s.caCert, &key.PublicKey, s.caKey))
	digest := sha256.Sum256(artifact)
	sig := must(ecdsa.SignASN1(rand.Reader, key, digest[:]))
	b64 := base64.StdEncoding.EncodeToString
	body := map[string]any{
		"apiVersion": "0.0.1", "kind": "hashedrekord",
		"spec": map[string]any{
			"data": map[string]any{"hash": map[string]any{"algorithm": "sha256", "value": hex.EncodeToString(digest[:])}},
			"signature": map[string]any{"content": b64(sig), "publicKey": map[string]any{
				"content": b64(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}))}},
		},
	}
	bodyB64 := b64(must(json.Marshal(body)))
	s.nextIdx++
	idx, secs := s.nextIdx, at.Unix()
	payload := must(json.Marshal(struct {
		Body           string `json:"body"`
		IntegratedTime int64  `json:"integratedTime"`
		LogID          string `json:"logID"`
		LogIndex       int64  `json:"logIndex"`
	}{bodyB64, secs, hex.EncodeToString(s.logID), idx}))
	psum := sha256.Sum256(payload)
	set := must(ecdsa.SignASN1(rand.Reader, s.logKey, psum[:]))
	bundle := map[string]any{
		"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{
			"certificate": map[string]any{"rawBytes": b64(leafDER)},
			"tlogEntries": []any{map[string]any{
				"logIndex": strconv.FormatInt(idx, 10), "logId": map[string]any{"keyId": b64(s.logID)},
				"kindVersion":       map[string]any{"kind": "hashedrekord", "version": "0.0.1"},
				"integratedTime":    strconv.FormatInt(secs, 10),
				"inclusionPromise":  map[string]any{"signedEntryTimestamp": b64(set)},
				"canonicalizedBody": bodyB64,
			}},
		},
		"messageSignature": map[string]any{
			"messageDigest": map[string]any{"algorithm": "SHA2_256", "digest": b64(digest[:])},
			"signature":     b64(sig),
		},
	}
	return must(json.Marshal(bundle))
}

// Spec is a bundle to write.
type Spec struct {
	Name, Version, Publisher string
	Abhed                    string
	Files                    map[string]string
	Hooks                    []plugin.Hook
	MCPServers               []plugin.MCPServer
	// Capabilities, when set, are written as given instead of computed.
	Capabilities *plugin.Capabilities
}

// Write packs spec into dir (made fresh) and returns the manifest's bytes.
func Write(dir string, spec Spec) []byte {
	if spec.Version == "" {
		spec.Version = "1.0.0"
	}
	if spec.Publisher == "" {
		spec.Publisher = "Test Publisher"
	}
	files := map[string][]byte{}
	for p, c := range spec.Files {
		files[p] = []byte(c)
	}
	m := plugin.Manifest{Schema: 1, Name: spec.Name, Version: spec.Version, Publisher: spec.Publisher, Abhed: spec.Abhed,
		Hooks: spec.Hooks, MCPServers: spec.MCPServers}
	var data []byte
	if spec.Capabilities != nil {
		m.Capabilities = *spec.Capabilities
		for _, p := range sortedKeys(files) {
			m.Files = append(m.Files, plugin.File{Path: p, SHA256: plugin.Digest(files[p])})
		}
		data = append(must(json.MarshalIndent(m, "", "  ")), '\n')
	} else {
		data = must(plugin.Pack(m, files))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		panic(err)
	}
	files[plugin.ManifestName] = data
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			panic(err)
		}
		if err := os.WriteFile(full, c, 0o600); err != nil {
			panic(err)
		}
	}
	return data
}

// SignDir signs dir's manifest and writes the signature beside it.
func (s *Sigstore) SignDir(dir, identity, issuer string) {
	m := must(os.ReadFile(filepath.Join(dir, plugin.ManifestName))) // #nosec G304 -- a test's own bundle
	if err := os.WriteFile(filepath.Join(dir, plugin.SignatureName), s.Sign(m, identity, issuer), 0o600); err != nil {
		panic(err)
	}
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Archive is dir as a gzipped tar under one top folder, as `tar czf` makes.
func Archive(dir string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	top := filepath.Base(dir)
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		data, err := os.ReadFile(p) // #nosec G304 G122 -- a test's own bundle
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: top + "/" + filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		panic(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// InMemory packs spec as a gzipped tar without touching the disk, and
// returns it with its manifest's digest.
func InMemory(spec Spec) (archive []byte, digest string) {
	if spec.Version == "" {
		spec.Version = "1.0.0"
	}
	if spec.Publisher == "" {
		spec.Publisher = "Test Publisher"
	}
	files := map[string][]byte{}
	for p, c := range spec.Files {
		files[p] = []byte(c)
	}
	m := plugin.Manifest{Schema: 1, Name: spec.Name, Version: spec.Version, Publisher: spec.Publisher, Abhed: spec.Abhed,
		Hooks: spec.Hooks, MCPServers: spec.MCPServers}
	data := must(plugin.Pack(m, files))
	files[plugin.ManifestName] = data
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, p := range sortedKeys(files) {
		if err := tw.WriteHeader(&tar.Header{Name: spec.Name + "/" + p, Mode: 0o644, Size: int64(len(files[p])), Typeflag: tar.TypeReg}); err != nil {
			panic(err)
		}
		_, _ = tw.Write(files[p])
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes(), plugin.Digest(data)
}
