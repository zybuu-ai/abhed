package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

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
	return resolveKey(p, keyGuard{})
}

// keyGuard is what a provider's key may be taken from: workspace marks an
// entry a workspace's file defined, mayStore judges a stored key no hosted
// type owns.
type keyGuard struct {
	workspace bool
	mayStore  func(key string) error
}

// keyDestinations are every input that decides where p's key is sent, as
// endpointUnbound lists them: the type (watsonx sends it to IBM's token URL
// when it names none), the base_url's https host or, with no base_url, the
// type's own endpoint, watsonx's IAM token URL (the field or extra.iam_url),
// and a vertex type's region and project. The host is "" for an endpoint
// that is not https on the default port; such an entry is never matched.
func keyDestinations(p ProviderConfig) [6]string {
	host := modelkeys.EndpointHost(p.Type, p.BaseURL)
	if host == "" && strings.TrimSpace(p.BaseURL) == "" {
		host = "the own endpoint of " + p.Type
	}
	d := [6]string{host, p.IAMURL, p.Extra["iam_url"], "", "", p.Type}
	if strings.HasPrefix(p.Type, "vertex-") {
		d[3], d[4] = p.Region, p.ProjectID
	}
	return d
}

// hostedKeyRefused says why a hosted type's key (ANTHROPIC_API_KEY, ...) may
// not go to p: only to that type's official host, and never with a token URL
// that would be sent it too. "" when it may, or name is no hosted key.
func hostedKeyRefused(name string, p ProviderConfig) string {
	ok, hosted := modelkeys.HostedKeyAllowed(name, p.Type, p.BaseURL)
	if !hosted {
		return ""
	}
	if ok && p.IAMURL == "" && p.Extra["iam_url"] == "" {
		return ""
	}
	return fmt.Sprintf("the key %s is kept for %s's own endpoint and is not sent to %s",
		Printable(name), modelkeys.HostedTypeOfKey(name), Printable(orHost(modelkeys.EndpointHost(p.Type, p.BaseURL), p.BaseURL)))
}

// resolveKeyNamed is ResolveKey for the configured provider name. An entry a
// workspace's file defined gets a hosted key, stored or exported, only at the
// type's own endpoint, and another stored key only when the person's own or
// the managed configuration names that key for the very same destinations.
func (c Config) resolveKeyNamed(name string, p ProviderConfig) ProviderConfig {
	if !c.WorkspaceProviders[name] {
		return ResolveKey(p)
	}
	return resolveKey(p, keyGuard{workspace: true, mayStore: func(key string) error {
		want := keyDestinations(p)
		for n, q := range c.Model.Providers {
			if c.WorkspaceProviders[n] || want[0] == "" {
				continue
			}
			if q.KeyName() == key && keyDestinations(q) == want {
				return nil
			}
		}
		return fmt.Errorf("model %s comes from the workspace's file, which may not have the stored key %s sent to %s; "+
			"only a model in your own configuration with the same endpoint (and token URL) can use it", Printable(name), Printable(key), Printable(orHost(want[0], p.BaseURL)))
	}})
}

func orHost(host, base string) string {
	if host != "" {
		return host
	}
	return base
}

// resolveKey is ResolveKey under g.
func resolveKey(p ProviderConfig, g keyGuard) ProviderConfig {
	if p.APIKey != "" {
		p.KeySource = KeySource{Kind: modelkeys.KindConfig}
		secrets.AlsoRedact("MODEL_KEY", p.APIKey)
		return p
	}
	if env := p.envKeyName(); env != "" {
		if v := os.Getenv(env); v != "" {
			// A workspace entry is not given an exported hosted key away from its host either.
			if why := hostedKeyRefused(env, p); g.workspace && why != "" {
				p.KeySource = KeySource{Name: env, Err: errors.New(why)}
				return p
			}
			p.APIKey, p.KeySource = v, KeySource{Kind: modelkeys.KindEnv, Name: env}
			secrets.AlsoRedact(env, v)
			return p
		}
	}
	name := p.KeyName()
	if name == "" {
		return p
	}
	if why := hostedKeyRefused(name, p); why != "" {
		p.KeySource = KeySource{Name: name, Err: errors.New(why)}
		return p
	}
	if _, hosted := modelkeys.HostedByKey(name); !hosted && g.mayStore != nil {
		if err := g.mayStore(name); err != nil {
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
