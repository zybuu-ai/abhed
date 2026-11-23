// Package machinegrant keeps what a person let Abhed use of their own
// machine's setup: a kubeconfig context, an ~/.ssh/config host, a cluster
// login made from a pasted credential. A grant is kept in the person's own
// state (~/.abhed/state/machine-grants.json), never in a workspace, so a
// repository can neither add one nor read one, and every workspace the
// person opens shares it.
//
// A grant names what it covers by identity, not only by name: a context
// whose server, CA or credential helper changed is another context, and is
// asked about again.
package machinegrant

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/filelock"
	"github.com/zybuu-ai/abhed/internal/nlink"
)

// Kinds of grant.
const (
	KindK8sContext = "k8s-context" // a kubeconfig context
	KindSSHHost    = "ssh-host"    // a Host in ~/.ssh/config
	KindK8sLogin   = "k8s-login"   // a cluster logged in to from a pasted token
)

// Grant is one kept grant.
type Grant struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Identity sums what the grant was given for; see Identity.
	Identity  string    `json:"identity"`
	What      string    `json:"what,omitempty"` // shown when listing, never a credential
	GrantedAt time.Time `json:"granted_at"`
	// Login is set for KindK8sLogin: where the token goes and how it is checked.
	Login *Login `json:"login,omitempty"`
}

// Login is a cluster a person logged in to by pasting a token: the server
// they pasted it with, the CA it is verified against, and the stored
// secret that holds it. The token itself is in the secrets store only.
type Login struct {
	Server string `json:"server"`
	// CAPEM verifies the server: the certificate the person approved, or ""
	// for the system roots.
	CAPEM string `json:"ca_pem,omitempty"`
	// CAFingerprint is the SHA-256 of the pinned certificate, as shown when approved.
	CAFingerprint string `json:"ca_fingerprint,omitempty"`
	Secret        string `json:"secret"`
	Namespace     string `json:"namespace,omitempty"`
}

// Identity sums parts into a grant identity. Parts are joined unambiguously.
func Identity(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s\x00", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

type file struct {
	Version int     `json:"version"`
	Grants  []Grant `json:"grants"`
}

// Path is where grants are kept; tests replace it.
var Path = func() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".abhed", "state", "machine-grants.json")
}

var mu sync.Mutex

// read loads the file. A missing file holds nothing; an unreadable one holds
// nothing and says why, so nothing is taken as granted from it.
func read() (file, error) {
	f := file{Version: 1}
	p := Path()
	if p == "" {
		return f, errors.New("there is no home folder to keep grants in")
	}
	if n, err := nlink.Linked(p); err != nil {
		return f, err
	} else if n > 0 {
		return f, nlink.Refusal(p, n)
	}
	b, err := os.ReadFile(p) // #nosec G304 -- the person's own state file
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return file{Version: 1}, fmt.Errorf("%s is not a list of grants: %w", p, err)
	}
	return f, nil
}

// change applies fn under the file's lock and writes the result back when fn
// says so. An unreadable file is set aside, not overwritten.
func change(fn func(*file) bool) error {
	p := Path()
	if p == "" {
		return errors.New("there is no home folder to keep grants in")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	unlock, err := filelock.Lock(p+".lock", 10*time.Second, "%s is held by another Abhed process (waited %s)")
	if err != nil {
		return err
	}
	defer unlock()
	f, err := read()
	bad := err != nil
	if bad {
		if _, statErr := os.Lstat(p); statErr != nil {
			return err
		}
	}
	if !fn(&f) {
		return nil
	}
	if bad {
		if err := os.Rename(p, p+".unreadable-"+time.Now().UTC().Format("20060102T150405")); err != nil {
			return err
		}
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	tmp, err := os.CreateTemp(filepath.Dir(p), ".machine-grants-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
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
	return os.Rename(tmp.Name(), p)
}

// Kept reports whether a grant of kind for name with identity is kept.
func Kept(kind, name, identity string) bool {
	f, err := read()
	if err != nil {
		return false
	}
	for _, g := range f.Grants {
		if g.Kind == kind && g.Name == name && g.Identity == identity {
			return true
		}
	}
	return false
}

// Keep keeps g, replacing one of the same kind and name.
func Keep(g Grant) error {
	if g.Kind == "" || g.Name == "" || g.Identity == "" {
		return errors.New("a grant needs a kind, a name and an identity")
	}
	if g.GrantedAt.IsZero() {
		g.GrantedAt = time.Now().UTC()
	}
	return change(func(f *file) bool {
		out := f.Grants[:0]
		for _, have := range f.Grants {
			if have.Kind != g.Kind || have.Name != g.Name {
				out = append(out, have)
			}
		}
		f.Grants = append(out, g)
		return true
	})
}

// Forget drops the grant of kind for name, and reports whether there was one.
func Forget(kind, name string) (bool, error) {
	found := false
	err := change(func(f *file) bool {
		out := f.Grants[:0]
		for _, have := range f.Grants {
			if have.Kind == kind && have.Name == name {
				found = true
				continue
			}
			out = append(out, have)
		}
		f.Grants = out
		return found
	})
	return found, err
}

// List returns the kept grants, sorted by kind and name.
func List() ([]Grant, error) {
	f, err := read()
	if err != nil {
		return nil, err
	}
	out := append([]Grant(nil), f.Grants...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Logins are the kept cluster logins, by name.
func Logins() map[string]Login {
	out := map[string]Login{}
	gs, err := List()
	if err != nil {
		return out
	}
	for _, g := range gs {
		if g.Kind == KindK8sLogin && g.Login != nil {
			out[g.Name] = *g.Login
		}
	}
	return out
}

// ValidKind reports a kind Forget and the CLI accept.
func ValidKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case KindK8sContext, KindSSHHost, KindK8sLogin:
		return true
	}
	return false
}
