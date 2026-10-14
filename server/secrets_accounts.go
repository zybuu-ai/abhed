package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/internal/secrets"
)

// accountSecrets keeps one secrets store per account on a server several
// accounts sign in to, so one account's sessions never read another's
// secrets. The operator's store serves scheduled runs only. Stores are files
// in secrets.AccountsDir named by a digest, so no account's name is on disk.
type accountSecrets struct {
	dir      string
	operator string
	// born says when a local account was made, so an account made later under
	// a removed one's name starts with an empty store; nil refuses local
	// accounts.
	born func(ctx context.Context, name string) (time.Time, error)
	// err refuses every account's store when the directory cannot be used.
	err error
}

// errNoAccount is a caller with no store of its own.
var errNoAccount = errors.New("this caller has no account to keep secrets for")

// defaultAccountSecrets is the per-account stores a server gives its
// sessions when accounts sign in and the embedder named no SecretsFor; nil
// when nobody signs in, where every session is the operator's.
func defaultAccountSecrets(opts Options) *accountSecrets {
	if opts.SecretsFor != nil || opts.Config.Auth.Mode == "" || opts.Config.Auth.Mode == "none" {
		return nil
	}
	a := &accountSecrets{born: localBorn(opts.Auth)}
	a.dir, a.err = secrets.AccountsDir()
	if a.err == nil && !filepath.IsAbs(a.dir) {
		a.err = fmt.Errorf("%s must be an absolute path, not %q", secrets.EnvAccountsDir, a.dir)
	}
	if p, err := secrets.DefaultPath(); err == nil {
		a.operator = p
	} else if a.err == nil {
		a.err = err
	}
	return a
}

// localBorn reads when a local account was made; nil without local accounts.
func localBorn(mw *auth.Middleware) func(context.Context, string) (time.Time, error) {
	if mw == nil {
		return nil
	}
	for _, p := range mw.Providers {
		if l, ok := p.(*auth.LocalAuth); ok && l.Store != nil {
			return func(ctx context.Context, name string) (time.Time, error) {
				u, err := l.Store.Get(ctx, strings.ToLower(name))
				if err == nil && u == nil {
					err = errors.New("no such account")
				}
				if err != nil {
					return time.Time{}, err
				}
				return u.CreatedAt, nil
			}
		}
	}
	return nil
}

// For is the store a session started for user in tenant reads. A caller
// with no account, such as an anonymous one, gets an empty store nobody can
// write: its sessions run with no secrets.
func (a *accountSecrets) For(tenant, user string) (*secrets.Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path, operator, err := a.path(ctx, tenant, user)
	if errors.Is(err, errNoAccount) {
		return secrets.OpenSource("no account's secrets", noSecrets{}), nil
	}
	if err != nil {
		return nil, err
	}
	if operator {
		return secrets.Open(path), nil
	}
	return secrets.Open(path).AddedWith(accountAddHint), nil
}

// accountAddHint is how an account adds a secret its session lacks.
const accountAddHint = "add it to your account's own store with `PUT /v1/secrets/%s`"

// noSecrets is the store of a caller with no account: empty, and not written.
type noSecrets struct{}

func (noSecrets) Load(context.Context) (map[string]string, error) { return map[string]string{}, nil }
func (noSecrets) Set(context.Context, string, string) error {
	return errNoAccount
}
func (noSecrets) Remove(context.Context, string) error { return errNoAccount }

// path is user's store, and whether it is the operator's.
func (a *accountSecrets) path(ctx context.Context, tenant, user string) (string, bool, error) {
	if a.err != nil {
		return "", false, fmt.Errorf("per-account secrets: %w", a.err)
	}
	switch {
	case strings.HasPrefix(user, auth.SchedulePrefix):
		return a.operator, true, nil // a run the operator's configuration started
	case user == "" || user == auth.Anonymous || auth.OwnsNothing(user) || strings.HasPrefix(user, auth.UnclaimedPrefix):
		return "", false, errNoAccount
	}
	if tenant == "" {
		tenant = "default"
	}
	key := tenant + "\x00" + user
	if name, ok := strings.CutPrefix(user, auth.ProviderLocal+":"); ok {
		if a.born == nil {
			return "", false, errNoAccount
		}
		born, err := a.born(ctx, name)
		if err != nil {
			return "", false, fmt.Errorf("the account %s: %w", name, err)
		}
		if !born.IsZero() {
			key += "\x00" + strconv.FormatInt(born.UnixMicro(), 10)
		}
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(a.dir, hex.EncodeToString(sum[:])+".json"), false, nil
}

// maxSecretValue bounds one value: a key file or certificate fits.
const maxSecretValue = 64 << 10

// mount registers each account's own secrets routes: names listed, values
// set and removed, never read back. Nobody reaches another account's store.
func (a *accountSecrets) mount(s *Server, mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		v := a.own(s, w, r)
		if v == nil {
			return
		}
		names, err := v.Names()
		if err != nil {
			s.log.Error("secrets: list", "user", UserOf(r.Context()), "err", err)
			WriteError(w, http.StatusInternalServerError, "your secrets cannot be read")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"names": names})
	})
	mux.HandleFunc("PUT /v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !secrets.ValidName(name) {
			WriteError(w, http.StatusBadRequest, "secret names are upper-case identifiers such as GITHUB_TOKEN")
			return
		}
		var req struct {
			Value string `json:"value"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSecretValue+1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			WriteError(w, http.StatusBadRequest, `the body is {"value": "..."}, under 64 KiB`)
			return
		}
		if n := utf8.RuneCountInString(req.Value); strings.TrimSpace(req.Value) == "" || n < secrets.MinLength {
			WriteError(w, http.StatusBadRequest, fmt.Sprintf("a secret is at least %d characters", secrets.MinLength))
			return
		}
		if len(req.Value) > maxSecretValue {
			WriteError(w, http.StatusRequestEntityTooLarge, "a secret is at most 64 KiB")
			return
		}
		v := a.own(s, w, r)
		if v == nil {
			return
		}
		if err := v.Set(name, req.Value); err != nil {
			s.log.Error("secrets: store", "user", UserOf(r.Context()), "name", name, "err", err)
			WriteError(w, http.StatusInternalServerError, "the secret could not be stored")
			return
		}
		s.log.Info("secret stored", "user", UserOf(r.Context()), "name", name)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		v := a.own(s, w, r)
		if v == nil {
			return
		}
		name := r.PathValue("name")
		if err := v.Remove(name); err != nil {
			s.log.Error("secrets: remove", "user", UserOf(r.Context()), "name", name, "err", err)
			WriteError(w, http.StatusInternalServerError, "the secret could not be removed")
			return
		}
		s.log.Info("secret removed", "user", UserOf(r.Context()), "name", name)
		w.WriteHeader(http.StatusNoContent)
	})
}

// own is the caller's store, or answers why there is none to change here.
func (a *accountSecrets) own(s *Server, w http.ResponseWriter, r *http.Request) *secrets.Store {
	path, operator, err := a.path(r.Context(), TenantOf(r.Context()), UserOf(r.Context()))
	switch {
	case err != nil:
		s.log.Warn("secrets: no store for the caller", "user", UserOf(r.Context()), "err", err)
		WriteError(w, http.StatusForbidden, "you have no account to keep secrets for")
		return nil
	case operator:
		WriteError(w, http.StatusForbidden, "a scheduled run's secrets are the operator's: manage them with `abhed secret` on the host")
		return nil
	}
	return secrets.Open(path)
}
