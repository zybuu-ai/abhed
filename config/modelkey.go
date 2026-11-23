package config

import (
	"os"

	"github.com/zybuu-ai/abhed/internal/modelkeys"
	"github.com/zybuu-ai/abhed/internal/secrets"
)

// KeySource is where a provider's key was found: Kind is modelkeys'
// config, env, keychain or file, or "" for none; Name the variable or the
// stored name. Err is why a store could not be read.
type KeySource struct {
	Kind string
	Name string
	Err  error
}

// KeyName is the name p's key goes by: api_key_secret, else api_key_env, else
// the type's own name at its official endpoint; "" when it has none.
func (p ProviderConfig) KeyName() string {
	switch {
	case p.APIKeySecret != "":
		return p.APIKeySecret
	case p.APIKeyEnv != "":
		return p.APIKeyEnv
	}
	return modelkeys.DefaultName(p.Type, p.BaseURL)
}

// envKeyName is the variable p's key is read from: api_key_env, else the
// type's own name when the configuration names no key at all.
func (p ProviderConfig) envKeyName() string {
	if p.APIKeyEnv != "" {
		return p.APIKeyEnv
	}
	if p.APIKeySecret == "" {
		return modelkeys.DefaultName(p.Type, p.BaseURL)
	}
	return ""
}

// ResolveKey fills p.APIKey and p.KeySource: the key in the configuration,
// else the environment variable, else the key stored under KeyName. A key
// found is redacted by every redactor in this process from then on.
func ResolveKey(p ProviderConfig) ProviderConfig {
	if p.APIKey != "" {
		p.KeySource = KeySource{Kind: modelkeys.KindConfig}
		secrets.AlsoRedact("MODEL_KEY", p.APIKey)
		return p
	}
	if env := p.envKeyName(); env != "" {
		if v := os.Getenv(env); v != "" {
			p.APIKey, p.KeySource = v, KeySource{Kind: modelkeys.KindEnv, Name: env}
			secrets.AlsoRedact(env, v)
			return p
		}
	}
	name := p.KeyName()
	if name == "" {
		return p
	}
	v, kind, err := modelkeys.Lookup(name)
	if err != nil {
		p.KeySource = KeySource{Name: name, Err: err}
		return p
	}
	if v != "" {
		p.APIKey, p.KeySource = v, KeySource{Kind: kind, Name: name}
		return p
	}
	p.KeySource = KeySource{Name: name}
	return p
}
