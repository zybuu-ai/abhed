package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/modelkeys"
	"golang.org/x/term"
)

// `abhed provider`: a hosted provider's key, typed once without echo and
// kept in the macOS Keychain (else Abhed's secret store, mode 600), so a
// session started anywhere, Studio from the Dock included, finds it without
// an exported variable. The key is never written to configuration, printed,
// sent to the model or recorded.

// providerIO is where `abhed provider` reads the key and writes.
type providerIO struct {
	out, err io.Writer
	// readKey reads the key without echo; nil when there is no terminal.
	readKey func(prompt string) (string, error)
}

func stdProviderIO() providerIO {
	pio := providerIO{out: os.Stdout, err: os.Stderr}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		pio.readKey = func(prompt string) (string, error) {
			fmt.Fprint(os.Stderr, prompt)
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			return string(b), err
		}
	}
	return pio
}

func providerUsage(w io.Writer) int {
	types := make([]string, 0, len(modelkeys.HostedTypes()))
	for _, h := range modelkeys.HostedTypes() {
		types = append(types, h.Type)
	}
	fmt.Fprintln(w, "usage: abhed provider login TYPE|MODEL|KEY\n"+
		"       abhed provider logout TYPE|MODEL|KEY\n"+
		"       abhed provider status [-json]\n"+
		"  login asks for the key without echo and keeps it in the macOS Keychain, or where there is none in\n"+
		"  Abhed's secret store (mode 600). It is never written to config, shown, sent to the model or recorded.\n"+
		"  TYPE is one of "+strings.Join(types, ", ")+"; MODEL a configured model, whose key name is used;\n"+
		"  KEY a key name a configured model reads (api_key_secret or api_key_env), as provider status lists it.\n"+
		"  A variable set in the environment (ANTHROPIC_API_KEY, ...) still wins; abhed doctor says which was used.")
	return 2
}

// providerCmd is `abhed provider`.
func providerCmd(args []string, pio providerIO) int {
	fail := func(err error) int { fmt.Fprintf(pio.err, "abhed: %v\n", err); return 1 }
	if len(args) == 0 {
		return providerUsage(pio.err)
	}
	if len(args) > 1 && args[1] == "--" {
		args = append(args[:1:1], args[2:]...)
	}
	switch args[0] {
	case "login":
		if len(args) != 2 {
			return providerUsage(pio.err)
		}
		return fail0(providerLogin(args[1], pio), fail)
	case "logout":
		if len(args) != 2 {
			return providerUsage(pio.err)
		}
		return fail0(providerLogout(args[1], pio), fail)
	case "status":
		asJSON := false
		for _, a := range args[1:] {
			if strings.TrimLeft(a, "-") != "json" {
				return providerUsage(pio.err)
			}
			asJSON = true
		}
		return fail0(providerStatus(asJSON, pio.out), fail)
	}
	return providerUsage(pio.err)
}

// keyTarget is the key name for a type or a configured model, and the type
// it is for.
func keyTarget(arg string) (name, typ string, err error) {
	if h, ok := modelkeys.HostedType(arg); ok {
		return h.Key, h.Type, nil
	}
	if h, ok := modelkeys.HostedByKey(arg); ok {
		return h.Key, h.Type, nil
	}
	own, mgd, err := modelLayers()
	if err != nil {
		return "", "", err
	}
	// A key name a configured model reads, as provider status lists it.
	for _, c := range []config.Config{mgd, own} {
		for _, p := range c.Model.Providers {
			if p.KeyName() == arg && modelkeys.ValidName(arg) {
				if _, named := c.Model.Providers[arg]; !named {
					return arg, p.Type, nil
				}
			}
		}
	}
	for _, c := range []config.Config{mgd, own} {
		if p, ok := c.Model.Providers[arg]; ok {
			if p.APIKey != "" && p.APIKeyEnv == "" && p.APIKeySecret == "" {
				return "", "", fmt.Errorf("model %s has its key written in the configuration; remove api_key there and set api_key_secret, then log in", config.Printable(arg))
			}
			if n := p.KeyName(); n != "" {
				return n, p.Type, nil
			}
			return "", "", fmt.Errorf("model %s names no key (api_key_secret or api_key_env), and its endpoint is not its type's own", config.Printable(arg))
		}
	}
	types := make([]string, 0, len(modelkeys.HostedTypes()))
	for _, h := range modelkeys.HostedTypes() {
		types = append(types, h.Type)
	}
	return "", "", fmt.Errorf("%q is neither a hosted provider type (%s) nor a configured model", config.Printable(arg), strings.Join(types, ", "))
}

func providerLogin(arg string, pio providerIO) error {
	name, typ, err := keyTarget(arg)
	if err != nil {
		return err
	}
	if !modelkeys.ValidName(name) {
		return fmt.Errorf("%s cannot be stored: a key name is upper-case letters, digits and _", config.Printable(name))
	}
	if pio.readKey == nil {
		return errors.New("the key is read without echo at a terminal; run this in one")
	}
	where := modelkeys.Label(modelkeys.Where())
	key, err := pio.readKey(fmt.Sprintf("API key for %s (%s; kept in %s, not echoed): ", typ, name, where))
	if err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("no key was typed; nothing was stored")
	}
	kind, err := modelkeys.Store(name, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(pio.out, "stored %s in %s; new sessions use it\n", name, modelkeys.Label(kind))
	if env := os.Getenv(name); env != "" && env != key {
		fmt.Fprintf(pio.out, "note: %s is also set in this shell's environment, to a different key, and wins while it is set; `unset %s` to use the stored one\n", name, name)
	}
	_ = writeConfigChange(configChange{By: "provider login", Key: "model key", Name: name, Kind: kind, File: userConfigFile()})
	return nil
}

func providerLogout(arg string, pio providerIO) error {
	name, _, err := keyTarget(arg)
	if err != nil {
		return err
	}
	from, err := modelkeys.Remove(name)
	if err != nil {
		return err
	}
	if len(from) == 0 {
		fmt.Fprintf(pio.out, "no key named %s was stored\n", name)
		return nil
	}
	labels := make([]string, 0, len(from))
	for _, k := range from {
		labels = append(labels, modelkeys.Label(k))
	}
	fmt.Fprintf(pio.out, "removed %s from %s\n", name, strings.Join(labels, " and "))
	_ = writeConfigChange(configChange{By: "provider logout", Key: "model key", Name: name, File: userConfigFile()})
	return nil
}

// keyStatus is one key in `abhed provider status`: where it is found, never
// the key.
type keyStatus struct {
	Type   string   `json:"type"`
	Key    string   `json:"key"`
	Source string   `json:"source"`
	Models []string `json:"models,omitempty"`
}

func providerStatus(asJSON bool, out io.Writer) error {
	own, mgd, err := modelLayers()
	if err != nil {
		return err
	}
	byKey := map[string]*keyStatus{}
	var order []string
	add := func(typ, key string) *keyStatus {
		if s, ok := byKey[key]; ok {
			return s
		}
		s := &keyStatus{Type: typ, Key: key}
		byKey[key] = s
		order = append(order, key)
		return s
	}
	for _, h := range modelkeys.HostedTypes() {
		add(h.Type, h.Key)
	}
	var extra []string
	all := map[string]config.ProviderConfig{}
	for n, p := range own.Model.Providers {
		all[n] = p
	}
	for n, p := range mgd.Model.Providers {
		all[n] = p
	}
	for n := range all {
		extra = append(extra, n)
	}
	sort.Strings(extra)
	for _, n := range extra {
		p := all[n]
		if k := p.KeyName(); k != "" && modelkeys.ValidName(k) {
			s := add(p.Type, k)
			s.Models = append(s.Models, n)
		}
	}
	rows := make([]keyStatus, 0, len(order))
	for _, k := range order {
		s := byKey[k]
		s.Source = "none"
		if os.Getenv(k) != "" {
			s.Source = modelkeys.KindEnv
		} else if v, kind, err := modelkeys.Lookup(k); err == nil && v != "" {
			s.Source = kind
		}
		rows = append(rows, *s)
	}
	if asJSON {
		b, _ := json.Marshal(map[string]any{"providers": rows, "store": modelkeys.Where()})
		fmt.Fprintln(out, string(b))
		return nil
	}
	fmt.Fprintf(out, "a key stored now goes to %s\n", modelkeys.Label(modelkeys.Where()))
	for _, r := range rows {
		models := ""
		if len(r.Models) > 0 {
			models = "  (" + strings.Join(r.Models, ", ") + ")"
		}
		fmt.Fprintf(out, "%-12s %-22s %s%s\n", r.Type, r.Key, sourceWord(r.Source), models)
	}
	return nil
}

// userConfigFile is the person's own configuration file.
func userConfigFile() string {
	home, _ := os.UserHomeDir()
	return home + "/.abhed/config.json"
}
