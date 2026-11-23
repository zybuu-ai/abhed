package config

import (
	"fmt"
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

// ResolveKey fills p.APIKey and p.KeySource for a provider from the
// person's own or the managed configuration: the key in the configuration,
// else the environment variable, else the key stored under KeyName. A hosted
// type's stored key goes only to that type's official host. A key found is
// redacted by every redactor in this process from then on.
func ResolveKey(p ProviderConfig) ProviderConfig {
	return resolveKey(p, func(string) error { return nil })
}

// resolveKeyNamed is ResolveKey for the configured provider name: one a
// workspace's file defined is given a stored key only for the host the
// person's own or the managed configuration names for that key.
func (c Config) resolveKeyNamed(name string, p ProviderConfig) ProviderConfig {
	if !c.WorkspaceProviders[name] {
		return ResolveKey(p)
	}
	return resolveKey(p, func(key string) error {
		host := modelkeys.EndpointHost(p.Type, p.BaseURL)
		for n, q := range c.Model.Providers {
			if c.WorkspaceProviders[n] || host == "" {
				continue
			}
			if q.KeyName() == key && modelkeys.EndpointHost(q.Type, q.BaseURL) == host {
				return nil
			}
		}
		return fmt.Errorf("model %s comes from the workspace's file, which may not have the stored key %s sent to %s; "+
			"only a model in your own configuration at that host can use it", Printable(name), Printable(key), Printable(orHost(host, p.BaseURL)))
	})
}

func orHost(host, base string) string {
	if host != "" {
		return host
	}
	return base
}

// resolveKey is ResolveKey with mayStore judging a stored key that no hosted
// type owns.
func resolveKey(p ProviderConfig, mayStore func(key string) error) ProviderConfig {
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
	if ok, hosted := modelkeys.HostedKeyAllowed(name, p.Type, p.BaseURL); !ok {
		p.KeySource = KeySource{Name: name, Err: fmt.Errorf("the stored key %s is kept for %s's own endpoint and is not sent to %s",
			Printable(name), modelkeys.HostedTypeOfKey(name), Printable(orHost(modelkeys.EndpointHost(p.Type, p.BaseURL), p.BaseURL)))}
		return p
	} else if !hosted {
		if err := mayStore(name); err != nil {
			p.KeySource = KeySource{Name: name, Err: err}
			return p
		}
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
