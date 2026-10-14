// Package pki issues a small private certificate authority for one Abhed
// deployment: a CA, server certificates for `abhed serve`, and client
// certificates that sign a local account in over mutual TLS.
//
// It is for a deployment that has no PKI of its own. Keys are written 0600 in
// a 0700 directory and never printed; nothing overwrites an existing file
// unless asked to.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// KeyType names the key algorithm of a certificate.
type KeyType string

const (
	ECDSA KeyType = "ecdsa" // P-256, the default
	RSA   KeyType = "rsa"   // 3072 bits, for clients that cannot do ECDSA
)

// Validity defaults. A server certificate stays under the 398 days browsers
// accept; a CA outlives the certificates it issues.
const (
	CAValidity     = 5 * 365 * 24 * time.Hour
	ServerValidity = 397 * 24 * time.Hour
	ClientValidity = 365 * 24 * time.Hour
)

// File names inside a PKI directory.
const (
	CACert = "ca.pem"
	CAKey  = "ca-key.pem"
)

// ErrExists is returned when a file would be overwritten without Force.
var ErrExists = errors.New("already exists")

// Options are shared by every issuing call.
type Options struct {
	Dir     string
	Key     KeyType
	Days    int  // 0 uses the default validity for the kind of certificate
	Force   bool // replace existing files
	Name    string
	DNS     []string
	IPs     []net.IP
	Subject string // client certificates: the local account it signs in
	// now is overridable by tests.
	now func() time.Time
}

func (o Options) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

func (o Options) validity(def time.Duration) time.Duration {
	if o.Days > 0 {
		return time.Duration(o.Days) * 24 * time.Hour
	}
	return def
}

// Issued describes what was written, without the key.
type Issued struct {
	CertFile    string
	KeyFile     string
	Subject     string
	NotAfter    time.Time
	Fingerprint string // SHA-256 of the DER certificate
}

// InitCA creates a CA in o.Dir.
func InitCA(o Options) (Issued, error) {
	if err := ensureDir(o.Dir); err != nil {
		return Issued{}, err
	}
	certPath, keyPath := filepath.Join(o.Dir, CACert), filepath.Join(o.Dir, CAKey)
	if err := checkFree(o.Force, certPath, keyPath); err != nil {
		return Issued{}, err
	}
	key, err := newKey(o.Key)
	if err != nil {
		return Issued{}, err
	}
	name := o.Name
	if name == "" {
		name = "Abhed local CA"
	}
	now := o.clock()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: name, Organization: []string{"Abhed"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(o.validity(CAValidity)),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// The CA signs leaves only, never another CA.
		MaxPathLen:     0,
		MaxPathLenZero: true,
	}
	tmpl.SubjectKeyId = keyID(key.Public())
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return Issued{}, err
	}
	return write(certPath, keyPath, der, key)
}

// IssueServer writes <name>.pem and <name>-key.pem, signed by the CA in o.Dir.
func IssueServer(o Options) (Issued, error) {
	if len(o.DNS) == 0 && len(o.IPs) == 0 {
		return Issued{}, errors.New("a server certificate needs at least one -dns name or -ip address")
	}
	name := o.Name
	if name == "" {
		name = "server"
		if len(o.DNS) > 0 {
			name = o.DNS[0]
		}
	}
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: firstOr(o.DNS, name)},
		DNSNames:    o.DNS,
		IPAddresses: o.IPs,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	return issue(o, name, tmpl, ServerValidity)
}

// validSubject matches a local account name, the same rule accounts use.
var validSubject = regexp.MustCompile(`^[a-zA-Z0-9._-]{2,64}$`)

// IssueClient writes a client certificate whose common name is o.Subject, the
// local account it signs in when the server maps client certificates.
func IssueClient(o Options) (Issued, error) {
	if !validSubject.MatchString(o.Subject) {
		return Issued{}, fmt.Errorf("client certificate subject %q is not an account name (2-64 letters, digits, '.', '_' or '-')", o.Subject)
	}
	name := o.Name
	if name == "" {
		name = "client-" + o.Subject
	}
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: o.Subject},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return issue(o, name, tmpl, ClientValidity)
}

var validFileName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func issue(o Options, name string, tmpl *x509.Certificate, def time.Duration) (Issued, error) {
	if !validFileName.MatchString(name) || strings.HasPrefix(name, ".") || name == "ca" {
		return Issued{}, fmt.Errorf("name %q is not usable as a file name here", name)
	}
	caCert, caKey, err := LoadCA(o.Dir)
	if err != nil {
		return Issued{}, err
	}
	certPath := filepath.Join(o.Dir, name+".pem")
	keyPath := filepath.Join(o.Dir, name+"-key.pem")
	if err := checkFree(o.Force, certPath, keyPath); err != nil {
		return Issued{}, err
	}
	key, err := newKey(o.Key)
	if err != nil {
		return Issued{}, err
	}
	now := o.clock()
	tmpl.SerialNumber = serial()
	tmpl.NotBefore = now.Add(-5 * time.Minute)
	tmpl.NotAfter = now.Add(o.validity(def))
	if tmpl.NotAfter.After(caCert.NotAfter) {
		// A leaf that outlives its CA stops verifying before its own date.
		tmpl.NotAfter = caCert.NotAfter
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	if _, ok := key.(*rsa.PrivateKey); ok {
		tmpl.KeyUsage |= x509.KeyUsageKeyEncipherment
	}
	tmpl.BasicConstraintsValid = true
	tmpl.AuthorityKeyId = caCert.SubjectKeyId
	tmpl.SubjectKeyId = keyID(key.Public())
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, key.Public(), caKey)
	if err != nil {
		return Issued{}, err
	}
	return write(certPath, keyPath, der, key)
}

// LoadCA reads the CA certificate and key from dir, refusing a key file other
// users can read.
func LoadCA(dir string) (*x509.Certificate, crypto.Signer, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, CACert)) // #nosec G304 -- the directory the operator named
	if err != nil {
		return nil, nil, fmt.Errorf("no CA in %s (run `abhed pki init` first): %w", dir, err)
	}
	keyPath := filepath.Join(dir, CAKey)
	if err := PrivateFile(keyPath); err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath) // #nosec G304 -- the directory the operator named, key checked private above
	if err != nil {
		return nil, nil, err
	}
	b, _ := pem.Decode(certPEM)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("%s holds no certificate", filepath.Join(dir, CACert))
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if !cert.IsCA {
		return nil, nil, fmt.Errorf("%s is not a CA certificate", filepath.Join(dir, CACert))
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, nil, fmt.Errorf("%s holds no key", keyPath)
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", keyPath, err)
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, nil, fmt.Errorf("%s: unsupported key", keyPath)
	}
	return cert, signer, nil
}

// PrivateFile refuses a key file that is a link or that other users can read
// or write. On Windows, where modes do not describe access, it only checks
// that the file exists.
func PrivateFile(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link; a private key must be a regular file", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is readable by other users (mode %o); run chmod 600 on it", path, fi.Mode().Perm())
	}
	return nil
}

func newKey(t KeyType) (crypto.Signer, error) {
	switch t {
	case "", ECDSA:
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case RSA:
		return rsa.GenerateKey(rand.Reader, 3072)
	}
	return nil, fmt.Errorf("unknown key type %q; use ecdsa or rsa", t)
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic("pki: system random source unavailable: " + err.Error())
	}
	return n.Add(n, big.NewInt(1))
}

func keyID(pub crypto.PublicKey) []byte {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(der)
	return sum[:20]
}

func firstOr(s []string, def string) string {
	if len(s) > 0 {
		return s[0]
	}
	return def
}

func ensureDir(dir string) error {
	if dir == "" {
		return errors.New("no directory given")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	// Others who can write here could swap the CA for their own.
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by other users (mode %o); use a directory only you can write", dir, fi.Mode().Perm())
	}
	return nil
}

func checkFree(force bool, paths ...string) error {
	if force {
		return nil
	}
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("%s %w; pass -force to replace it", p, ErrExists)
		}
	}
	return nil
}

func write(certPath, keyPath string, der []byte, key crypto.Signer) (Issued, error) {
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Issued{}, err
	}
	// The key first: a certificate without its key is harmless, the reverse is not.
	if err := writeFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		return Issued{}, err
	}
	if err := writeFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return Issued{}, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return Issued{}, err
	}
	sum := sha256.Sum256(der)
	return Issued{CertFile: certPath, KeyFile: keyPath, Subject: cert.Subject.CommonName,
		NotAfter: cert.NotAfter, Fingerprint: hex.EncodeToString(sum[:])}, nil
}

// writeFile replaces path atomically with data at mode, never through a link.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
