// Package secretstore opens the secrets store outside this module, for an
// edition that keeps a store per account and hands each to the server through
// server.Options.SecretsFor. The format, the file checks and the redaction are
// the ones `abhed secret` uses.
package secretstore

import "github.com/zybuu-ai/abhed/internal/secrets"

// Store is a file of named values, readable by its owner only.
type Store = secrets.Store

// EnvFile and EnvAccountsDir name the variables that move the operator's
// store and the per-account directory.
const (
	EnvFile        = secrets.EnvFile
	EnvAccountsDir = secrets.EnvAccountsDir
)

// MinLength is the fewest characters `abhed secret set` accepts.
const MinLength = secrets.MinLength

// Open opens the store at path; a missing file is an empty store.
func Open(path string) *Store { return secrets.Open(path) }

// DefaultPath is the operator's store, the one `abhed secret` manages.
func DefaultPath() (string, error) { return secrets.DefaultPath() }

// AccountsDir is the directory per-account stores belong in: guarded as state,
// so the agent's tools and sandbox cannot reach it.
func AccountsDir() (string, error) { return secrets.AccountsDir() }

// ValidName reports whether name is one a secret can be stored under.
func ValidName(name string) bool { return secrets.ValidName(name) }
