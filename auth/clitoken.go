package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// CLITokenPrefix begins every token `abhed login` holds for a local account,
// so a verifier chain can tell it from an identity provider's.
const CLITokenPrefix = "abhcli_"

// CLITokens issues and verifies the bearer tokens the command line holds for
// a local account.
//
// A token is signed rather than stored: it names the account, the account's
// sign-out-everywhere count and a stamp of its password hash when issued, and
// is checked against the account as it stands on every request. So signing
// out everywhere, changing the password or removing the account ends every
// token at once, on every server sharing the account store and the key,
// without a token table. One token is ended early by `abhed logout`, which
// records its id until it would have expired anyway.
type CLITokens struct {
	Local *LocalAuth
	TTL   time.Duration

	key         []byte
	revokedPath string

	mu      sync.Mutex
	revoked map[string]int64 // token id -> expiry, unix seconds
	now     func() time.Time
}

type cliClaims struct {
	V       int    `json:"v"`
	ID      string `json:"jti"`
	Sub     string `json:"sub"`
	Rev     int64  `json:"rev"`
	PW      string `json:"pw"`
	Issued  int64  `json:"iat"`
	Expires int64  `json:"exp"`
}

// Token verification errors. They are returned to the client, so they say
// what to do and nothing about the key.
var (
	ErrNotCLIToken    = errors.New("not a command-line token")
	ErrCLITokenBad    = errors.New("the token is not valid here; run abhed login again")
	ErrCLITokenExpiry = errors.New("the token has expired; run abhed login again")
	ErrCLITokenEnded  = errors.New("the token was signed out; run abhed login again")
)

// OpenCLITokens loads the signing key at keyFile, creating it 0600 when
// absent. A key file other users can read, or that is a link, is refused.
func OpenCLITokens(local *LocalAuth, keyFile string, ttl time.Duration) (*CLITokens, error) {
	if local == nil {
		return nil, errors.New("command-line sign-in needs local accounts")
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	key, err := loadKey(keyFile)
	if err != nil {
		return nil, err
	}
	t := &CLITokens{Local: local, TTL: ttl, key: key, revokedPath: keyFile + ".revoked",
		revoked: map[string]int64{}, now: time.Now}
	if err := t.loadRevoked(); err != nil {
		return nil, err
	}
	return t, nil
}

func loadKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("no key file for command-line tokens")
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the operator's auth.cli.key_file
		if err != nil {
			return nil, fmt.Errorf("create the command-line token key: %w", err)
		}
		_, werr := f.WriteString(hex.EncodeToString(b) + "\n")
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, werr
		}
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("the command-line token key %s must be a regular file, not a link", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("the command-line token key %s is readable by other users (mode %o); run chmod 600 on it", path, fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator's auth.cli.key_file, checked above
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) < 32 {
		return nil, fmt.Errorf("the command-line token key %s is not 32 or more hex-encoded bytes", path)
	}
	return key, nil
}

// Issue signs a token for username, as the account stands now. A password
// that must be changed first, or an Admit hook's refusal, issues nothing.
func (t *CLITokens) Issue(ctx context.Context, username string) (token string, expires time.Time, err error) {
	u, err := t.Local.Store.Get(ctx, username)
	if err == nil && u == nil {
		err = ErrNoSuchUser
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if u.MustChange {
		return "", time.Time{}, errors.New("this account's password was set for it; change it in the browser first")
	}
	if t.Local.Admit != nil {
		if err := t.Local.Admit(ctx, u); err != nil {
			return "", time.Time{}, err
		}
	}
	stamp := passwordStamp(u.Hash)
	if stamp == "" {
		return "", time.Time{}, errors.New("this account has no password, so no token can be bound to it")
	}
	now := t.now()
	expires = now.Add(t.TTL)
	c := cliClaims{V: 1, ID: randomToken()[:22], Sub: u.Username, Rev: u.Revocations,
		PW: stamp, Issued: now.Unix(), Expires: expires.Unix()}
	body, err := json.Marshal(c)
	if err != nil {
		return "", time.Time{}, err
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return CLITokenPrefix + payload + "." + t.sign(payload), expires, nil
}

func (t *CLITokens) sign(payload string) string {
	m := hmac.New(sha256.New, t.key)
	m.Write([]byte("abhed-cli-token-v1\x00"))
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// parse checks the signature and expiry, and nothing about the account.
func (t *CLITokens) parse(token string) (cliClaims, error) {
	var c cliClaims
	if !strings.HasPrefix(token, CLITokenPrefix) {
		return c, ErrNotCLIToken
	}
	payload, sig, ok := strings.Cut(strings.TrimPrefix(token, CLITokenPrefix), ".")
	if !ok || len(token) > 2048 {
		return c, ErrCLITokenBad
	}
	if !hmac.Equal([]byte(sig), []byte(t.sign(payload))) {
		return c, ErrCLITokenBad
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(body, &c) != nil || c.V != 1 || c.Sub == "" || c.ID == "" {
		return c, ErrCLITokenBad
	}
	if t.now().Unix() >= c.Expires {
		return c, ErrCLITokenExpiry
	}
	return c, nil
}

// Verify implements TokenVerifier: a valid token for an account that has not
// signed out everywhere, changed its password or been removed since.
func (t *CLITokens) Verify(ctx context.Context, token string) (*Identity, error) {
	c, err := t.parse(token)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	_, ended := t.revoked[c.ID]
	t.mu.Unlock()
	if ended {
		return nil, ErrCLITokenEnded
	}
	u, err := t.Local.Store.Get(ctx, c.Sub)
	if errors.Is(err, ErrNoSuchUser) || (err == nil && u == nil) {
		return nil, ErrCLITokenEnded
	}
	if err != nil {
		return nil, ErrAccountUnchecked
	}
	if u.Revocations > c.Rev || passwordStamp(u.Hash) != c.PW {
		return nil, ErrCLITokenEnded
	}
	if u.MustChange {
		return nil, errors.New("this account's password was reset; change it in the browser, then run abhed login again")
	}
	if t.Local.Admit != nil {
		if err := t.Local.Admit(ctx, u); err != nil {
			return nil, err
		}
	}
	return &Identity{Subject: u.Username, Email: u.Email, Name: u.Name, Tenant: u.Tenant,
		Groups: slices.Clone(u.Groups), Provider: ProviderLocal,
		IssuedAt: c.Issued, Expires: c.Expires}, nil
}

// Revoke ends one token before its expiry. A token that does not verify
// needs no revoking and is not an error, so logout is idempotent.
func (t *CLITokens) Revoke(token string) error {
	c, err := t.parse(token)
	if err != nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now().Unix()
	for id, exp := range t.revoked {
		if exp <= now {
			delete(t.revoked, id)
		}
	}
	t.revoked[c.ID] = c.Expires
	return t.saveRevoked()
}

func (t *CLITokens) loadRevoked() error {
	b, err := os.ReadFile(t.revokedPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &t.revoked); err != nil {
		return fmt.Errorf("%s: %w", t.revokedPath, err)
	}
	return nil
}

func (t *CLITokens) saveRevoked() error {
	b, err := json.Marshal(t.revoked)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(t.revokedPath), "."+filepath.Base(t.revokedPath)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Rename(f.Name(), t.revokedPath)
}

// Verifiers asks each verifier in turn. One that answers ErrNotCLIToken, or
// ErrTokenNotMine, does not recognise the token and the next is asked; any
// other answer is final.
type Verifiers []TokenVerifier

// ErrTokenNotMine lets a verifier pass a token to the next in a Verifiers.
var ErrTokenNotMine = errors.New("token not recognised by this verifier")

func (vs Verifiers) Verify(ctx context.Context, token string) (*Identity, error) {
	last := ErrTokenNotMine
	for _, v := range vs {
		id, err := v.Verify(ctx, token)
		if errors.Is(err, ErrNotCLIToken) || errors.Is(err, ErrTokenNotMine) {
			last = err
			continue
		}
		return id, err
	}
	return nil, fmt.Errorf("the bearer token was not recognised: %w", last)
}

// ChainVerifier joins a verifier to the one already set; either may be nil.
// A command-line token verifier goes first, since it recognises its tokens
// by prefix and passes every other token on.
func ChainVerifier(have, add TokenVerifier) TokenVerifier {
	switch {
	case have == nil:
		return add
	case add == nil:
		return have
	}
	if _, ok := add.(*CLITokens); ok {
		return Verifiers{add, have}
	}
	return Verifiers{have, add}
}
