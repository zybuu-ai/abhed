package app

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/modelkeys"
	"github.com/zybuu-ai/abhed/internal/ollama"
)

// keyRefused reports a provider refusing the key: unauthorized, forbidden,
// payment required, or a request refused for the account's credit or billing.
func keyRefused(err error) bool {
	var se *model.StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired:
		return true
	case http.StatusBadRequest:
		b := strings.ToLower(se.Body)
		return strings.Contains(b, "credit balance") || strings.Contains(b, "billing") || strings.Contains(b, "insufficient_quota")
	}
	return false
}

// keySourceText says where p's key came from, never the key.
func keySourceText(p config.ProviderConfig) string {
	ks := p.KeySource
	switch ks.Kind {
	case modelkeys.KindConfig:
		return "the key written in the configuration (or ABHED_API_KEY)"
	case modelkeys.KindEnv:
		return "the key in $" + config.Printable(ks.Name)
	case modelkeys.KindKeychain, modelkeys.KindFile:
		return "the key " + config.Printable(ks.Name) + " stored in " + modelkeys.Label(ks.Kind)
	}
	if ks.Name != "" {
		return "no key: " + config.Printable(ks.Name) + " is neither set nor stored"
	}
	return "no key"
}

// keyMismatchHint is the note for a key the provider refused when it came
// from the environment and a different key is stored under the same name:
// the variable wins, so a stale export in one shell hides the stored key.
// "" for any other error or source.
func keyMismatchHint(err error, p config.ProviderConfig) string {
	if !keyRefused(err) || p.KeySource.Kind != modelkeys.KindEnv || p.KeySource.Name == "" {
		return ""
	}
	stored, kind, lerr := modelkeys.Lookup(p.KeySource.Name)
	if lerr != nil || stored == "" || stored == p.APIKey {
		return ""
	}
	name := config.Printable(p.KeySource.Name)
	return fmt.Sprintf("The key used came from $%s in this environment, which wins over the different key stored in %s. "+
		"If the stored key is the current one, run `unset %s` (or start Abhed where it is not exported); "+
		"otherwise store the new key with `abhed provider login %s`.", name, modelkeys.Label(kind), name, config.Printable(loginArg(p)))
}

// loginArg is what `abhed provider login` takes for p: its type when the key
// is the type's own name, else the key's name.
func loginArg(p config.ProviderConfig) string {
	if h, ok := modelkeys.HostedType(p.Type); ok && h.Key == p.KeyName() {
		return p.Type
	}
	return p.KeyName()
}

// sessionKeyHint is keyMismatchHint for the model an editor session runs
// on, after a blank line; "" when there is none.
func sessionKeyHint(s *acpSession, err error) string {
	if s == nil || !s.inner || !keyRefused(err) {
		return ""
	}
	name := s.parts.Config.Model.Default
	if m, ok := s.agent.(modelSwitcher); ok {
		if cur := currentModel(m.Models()); cur != "" {
			name = cur
		}
	}
	p, perr := s.parts.Config.ProviderNamed(name)
	if perr != nil {
		return ""
	}
	if hint := keyMismatchHint(err, p); hint != "" {
		return "\n\n" + hint
	}
	return ""
}

// modelKeyCheck is doctor's line on where the default model's key comes
// from, never the key; ok is false for a provider that takes no key.
func modelKeyCheck(p config.ProviderConfig) (status, detail string, ok bool) {
	ks := p.KeySource
	switch {
	case ks.Err != nil:
		return "warn", ks.Err.Error(), true
	case ks.Kind == modelkeys.KindEnv:
		return "ok", "from $" + config.Printable(ks.Name) + " in the environment (it wins over a stored key)", true
	case ks.Kind == modelkeys.KindKeychain || ks.Kind == modelkeys.KindFile:
		return "ok", config.Printable(ks.Name) + " from " + modelkeys.Label(ks.Kind), true
	case ks.Kind == modelkeys.KindConfig:
		return "ok", "written in the configuration (or ABHED_API_KEY); `abhed provider login` keeps it out of the file", true
	case ks.Name != "":
		return "warn", config.Printable(ks.Name) + " is neither set nor stored; run `abhed provider login " + config.Printable(loginArg(p)) + "`", true
	}
	return "", "", false
}

// noteLocalModelUse has each request to a model on this machine (chat,
// completion, next edit, commit message, embedding) count as use of the
// Ollama server Abhed may have started, from its start to its end, so the
// server is not stopped while in use.
func noteLocalModelUse() {
	f := ollama.Using
	model.LocalUse.Store(&f)
}

// adoptLegacyKeys has account id take over the model keys stored in the
// Keychain before sign-in, as it takes over the rest of that data (the secret
// store moves with its folder): every hosted type's key name and every name
// a configured model reads. It says what moved, or why not, on out.
func adoptLegacyKeys(id string, out io.Writer) {
	moved, err := modelkeys.AdoptLegacy(id, modelKeyNames())
	if err != nil {
		fmt.Fprintf(out, "The model keys from before sign-in were not all moved to your account (%v); abhed provider login stores one again.\n", err)
		return
	}
	if len(moved) > 0 {
		fmt.Fprintf(out, "Moved %d model key(s) in the Keychain to your account: %s.\n", len(moved), strings.Join(moved, ", "))
	}
}

// modelKeyNames are every hosted type's key name and every name a model in
// the person's own configuration reads.
func modelKeyNames() []string {
	names := modelkeys.HostedKeyNames()
	if own, err := userFileConfig(); err == nil {
		for _, p := range own.Model.Providers {
			if n := p.KeyName(); n != "" {
				names = append(names, n)
			}
		}
	}
	return names
}

// returnLegacyKeys moves account id's model keys in the Keychain back to the
// service from before sign-in, as unmigrate moves the rest back.
func returnLegacyKeys(id string, out io.Writer) {
	moved, err := modelkeys.ReturnLegacy(id, modelKeyNames())
	if err != nil {
		fmt.Fprintf(out, "The model keys were not all moved back from the account (%v); abhed provider login stores one again.\n", err)
		return
	}
	if len(moved) > 0 {
		fmt.Fprintf(out, "Moved %d model key(s) in the Keychain back: %s.\n", len(moved), strings.Join(moved, ", "))
	}
}
