package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// providerProblem is a provider's refusal as the person reads it in the chat:
// what the provider said, and the next step, naming configured models that
// could be used instead. Nil when err is not a provider's answer.
type providerProblem struct {
	*model.Problem
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Next     string `json:"next"`
	// Others are configured models whose key is at hand, to switch to.
	Others []string `json:"others,omitempty"`
}

// explainProvider reads err from provider name's model.
func explainProvider(cfg config.Config, name string, err error) *providerProblem {
	p := model.ProblemOf(err)
	if p == nil {
		return nil
	}
	pc := cfg.Model.Providers[name]
	out := &providerProblem{Problem: p, Provider: name, Model: pc.Model, Others: usableModels(cfg, name)}
	instead := ""
	if len(out.Others) > 0 {
		instead = " Switch to " + strings.Join(out.Others, " or ") + " with the model picker (or /model), or set model.default."
	}
	switch p.Kind {
	case model.ProblemKey:
		key := "the provider's key"
		if pc.APIKeyEnv != "" {
			key = "the key in $" + pc.APIKeyEnv
		}
		out.Next = "Check " + key + " and that it may use " + pc.Model + "." + instead
	case model.ProblemCredit:
		out.Next = "The account has no credit or quota left: add credit or raise the quota with the provider." + instead
	case model.ProblemRetired:
		out.Next = fmt.Sprintf("%s is retired or not available to this account; set another model for %s, or pick another.%s", pc.Model, name, instead)
	case model.ProblemRateLimit:
		out.Next = "The provider is rate limiting: wait a minute and send again." + instead
	case model.ProblemUnavailable:
		out.Next = "The provider failed on its side: try again shortly." + instead
	default:
		out.Next = "Run `abhed doctor` to check the configuration." + instead
	}
	return out
}

// Text is the problem as said in the chat.
func (p *providerProblem) Text() string {
	return fmt.Sprintf("The model provider refused the request (%s, %d): %s\nNext: %s", p.Provider, p.Status, p.Message, p.Next)
}

// usableModels are the configured, offered providers other than name whose
// key is at hand, or that need none, and that are not remembered unavailable.
func usableModels(cfg config.Config, name string) []string {
	var out []string
	cur := cfg.Model.Providers[name]
	for _, n := range toolset.OfferedModels(cfg) {
		p := cfg.Model.Providers[n]
		if n == name || p.Model == cur.Model && p.Type == cur.Type {
			continue
		}
		if p.APIKey == "" && p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) == "" {
			continue
		}
		if p.APIKeyEnv == "" && p.APIKey == "" && !p.OnThisMachine() && hostedLabel(p.BaseURL) == "hosted" {
			continue
		}
		if unavailableModel(n, p.Model) != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// ---- models a provider said are unavailable ----

// unavailableFile keeps the models a provider said were retired or not
// found, so a later session says so before its first call; tests set it.
var unavailableFile = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".abhed", "state", "models-unavailable.json")
}

var unavailableMu sync.Mutex

// unavailableEntry is one model a provider said was not available.
type unavailableEntry struct {
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// unavailableTTL is how long a remembered refusal is believed: a provider
// may bring a model back, or the account may gain access.
const unavailableTTL = 14 * 24 * time.Hour

func unavailableKey(provider, modelName string) string { return provider + "|" + modelName }

func readUnavailable() map[string]unavailableEntry {
	out := map[string]unavailableEntry{}
	if f := unavailableFile(); f != "" {
		if b, err := os.ReadFile(f); err == nil { // #nosec G304 -- Abhed's own state file
			_ = json.Unmarshal(b, &out)
		}
	}
	return out
}

func writeUnavailable(m map[string]unavailableEntry) {
	f := unavailableFile()
	if f == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := f + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, f)
	}
}

// noteProviderProblem remembers a retired or not-found model, and forgets
// one that answered.
func noteProviderProblem(p *providerProblem) {
	if p == nil || p.Kind != model.ProblemRetired {
		return
	}
	unavailableMu.Lock()
	defer unavailableMu.Unlock()
	m := readUnavailable()
	m[unavailableKey(p.Provider, p.Model)] = unavailableEntry{Message: p.Message, At: time.Now().UTC()}
	writeUnavailable(m)
}

// modelAnswered forgets that provider's model was unavailable.
func modelAnswered(provider, modelName string) {
	unavailableMu.Lock()
	defer unavailableMu.Unlock()
	m := readUnavailable()
	if _, ok := m[unavailableKey(provider, modelName)]; !ok {
		return
	}
	delete(m, unavailableKey(provider, modelName))
	writeUnavailable(m)
}

// unavailableModel is what the provider last said of a model it would not
// serve, within unavailableTTL, or nil.
func unavailableModel(provider, modelName string) *unavailableEntry {
	unavailableMu.Lock()
	defer unavailableMu.Unlock()
	e, ok := readUnavailable()[unavailableKey(provider, modelName)]
	if !ok || time.Since(e.At) > unavailableTTL {
		return nil
	}
	return &e
}

// defaultModelWarning says, before any call, that the default model was
// reported retired or not found, and names configured ones to use; "" when
// it was not.
func defaultModelWarning(cfg config.Config) string {
	name := cfg.Model.Default
	pc, ok := cfg.Model.Providers[name]
	if !ok {
		return ""
	}
	e := unavailableModel(name, pc.Model)
	if e == nil {
		return ""
	}
	w := fmt.Sprintf("the default model %s (%s) was reported unavailable by its provider on %s: %s.", name, pc.Model, e.At.Format("2 Jan"), e.Message)
	if others := usableModels(cfg, name); len(others) > 0 {
		w += " Pick another: " + strings.Join(others, ", ") + "."
	} else {
		w += " Configure another model (abhed init, or the model picker)."
	}
	return w
}
