// Package profiles is the command line's registry of Abhed servers it has
// signed in to: one profile per server, the current one, and an HTTP client
// that carries a profile's token and TLS settings.
//
// Profiles live in one file outside any workspace, ~/.abhed/profiles.json by
// default, under the directory the agent's tools and sandbox cannot reach.
// The file holds bearer tokens, so it is written 0600 and refused when other
// users can read it or when it is a link.
package profiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"time"
)

// EnvFile overrides where profiles are kept.
const EnvFile = "ABHED_PROFILES_FILE"

// Profile is one server and what this command line holds for it.
type Profile struct {
	URL string `json:"url"`
	// AuthMode is the server's auth.mode when last signed in.
	AuthMode string `json:"auth_mode,omitempty"`
	// Method is the sign-in method used: "local", or an identity provider's.
	Method    string    `json:"method,omitempty"`
	Username  string    `json:"username,omitempty"`
	Token     string    `json:"token,omitempty"`
	TokenType string    `json:"token_type,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// IssuedAt is when the token was received, to tell how much of its life
	// is left.
	IssuedAt     time.Time `json:"issued_at,omitzero"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	// TokenEndpoint and ClientID refresh a token that came with a refresh
	// token; RevocationEndpoint is told the token at logout.
	TokenEndpoint      string `json:"token_endpoint,omitempty"`
	RevocationEndpoint string `json:"revocation_endpoint,omitempty"`
	ClientID           string `json:"client_id,omitempty"`
	// CAFile is the only CA trusted for this server when set; CertFile and
	// KeyFile are a client certificate for a server that asks for one.
	CAFile   string `json:"ca_file,omitempty"`
	CertFile string `json:"cert_file,omitempty"`
	KeyFile  string `json:"key_file,omitempty"`
	// PendingLogout marks a profile signed out here whose server has not
	// yet recorded the sign-out; the token waits in PendingLogouts.
	PendingLogout bool `json:"pending_logout,omitempty"`
}

// SignedIn reports whether the profile holds a token that has not expired.
func (p *Profile) SignedIn(now time.Time) bool {
	return p.Token != "" && (p.ExpiresAt.IsZero() || now.Before(p.ExpiresAt))
}

// File is the profiles file.
type File struct {
	Version  int                 `json:"version"`
	Current  string              `json:"current,omitempty"`
	Profiles map[string]*Profile `json:"profiles"`
	// PendingLogouts are tokens signed out here whose server has not yet
	// recorded it; each is told again until it does.
	PendingLogouts []*Profile `json:"pending_logouts,omitempty"`
}

// Names lists the profiles in order.
func (f *File) Names() []string {
	out := make([]string, 0, len(f.Profiles))
	for n := range f.Profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidName reports whether n can name a profile.
func ValidName(n string) bool { return validName.MatchString(n) }

// DefaultPath is $ABHED_PROFILES_FILE, else ~/.abhed/profiles.json.
func DefaultPath() (string, error) {
	if p := os.Getenv(EnvFile); p != "" {
		return filepath.Abs(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".abhed", "profiles.json"), nil
}

// ErrNoProfile is returned when a named or current profile does not exist.
var ErrNoProfile = errors.New("no such profile")

// Load reads the file at path; a missing file is an empty registry.
func Load(path string) (*File, error) {
	f := &File{Version: 1, Profiles: map[string]*Profile{}}
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := private(path, fi); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the person's own profiles file, checked private above
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Profiles == nil {
		f.Profiles = map[string]*Profile{}
	}
	return f, nil
}

func private(path string, fi os.FileInfo) error {
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file, not a link", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s holds sign-in tokens and is open to other users (mode %o); run chmod 600 on it", path, fi.Mode().Perm())
	}
	return nil
}

// Save writes f to path, 0600, replacing it in one step.
func Save(path string, f *File) error {
	if f.Version == 0 {
		f.Version = 1
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil && (fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular()) {
		return fmt.Errorf("%s must be a regular file, not a link", path)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
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

// Get returns the named profile, or the current one when name is "".
func (f *File) Get(name string) (string, *Profile, error) {
	if name == "" {
		name = f.Current
	}
	if name == "" {
		return "", nil, fmt.Errorf("%w: no current profile; run abhed login URL", ErrNoProfile)
	}
	p, ok := f.Profiles[name]
	if !ok {
		return name, nil, fmt.Errorf("%w %q", ErrNoProfile, name)
	}
	return name, p, nil
}
