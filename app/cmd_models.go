package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/modelkeys"
)

// `abhed models`: the models in the person's own ~/.abhed/config.json, added
// and chosen without editing it. Every write goes through writeUserDoc
// (atomic, backed up, mode kept) under the file's lock; the workspace's file
// and the managed file are never written, and a setting the managed file
// makes is refused. Each change is appended to ~/.abhed/config-changes.jsonl.

// modelsIO is where `abhed models` reads its answer and writes.
type modelsIO struct {
	in       io.Reader
	out, err io.Writer
	terminal bool
	// byEditor is a yes read from -confirm-fd.
	byEditor bool
}

// modelID is a model id as a provider names it: an Ollama tag, a dated
// release, an org/model path.
var modelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$`)

// modelName is a configured provider's name.
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,63}$`)

// knownModel is what the built-in model table says of id served by type typ:
// whether it is listed, and its context window (0 when the table has none).
//
// Seam: the table of context windows belongs to internal/model and is being
// added on next130/fix-context-models. Until it lands, a model is known when
// it is priced, and no window is filled; the default window applies. Replace
// the body with the table's lookup then; nothing else here changes.
var knownModel = func(typ, id string) (known bool, window int) {
	if !pricedFamily(typ, id) {
		return false, 0
	}
	_, ok := model.PriceFor(id, nil)
	return ok, 0
}

// knownModels lists the built-in table's models for typ (see knownModel).
var knownModels = func(typ string) []string {
	var out []string
	for _, id := range model.PricedModels() {
		if pricedFamily(typ, id) {
			out = append(out, id)
		}
	}
	return out
}

// pricedFamily reports whether id is a model typ's own endpoint serves.
func pricedFamily(typ, id string) bool {
	switch typ {
	case "anthropic":
		return strings.HasPrefix(id, "claude-")
	case "openai":
		return strings.HasPrefix(id, "gpt-") || strings.HasPrefix(id, "o")
	case "gemini":
		return strings.HasPrefix(id, "gemini-")
	}
	return false
}

// localTypes are the provider types whose default endpoint is this machine.
var localTypes = []string{"ollama", "vllm", "llamacpp", "tgi", "sglang"}

// providerHosted reports whether p sends the code off this machine.
func providerHosted(p config.ProviderConfig) bool { return !p.OnThisMachine() }

func modelsUsage(w io.Writer) int {
	fmt.Fprintln(w, "usage: abhed models list [-json]\n"+
		"       abhed models add [-default] [-name NAME] [-json] PROVIDER MODEL...\n"+
		"       abhed models default [-check] [-confirm-fd N] [-json] NAME\n"+
		"       abhed models remove NAME\n"+
		"       abhed models known [-json] [TYPE]\n"+
		"  PROVIDER is a provider type (abhed providers lists them) or a model already configured,\n"+
		"  whose type, endpoint and key setting the new ones share. Each model is added under its own id.\n"+
		"  Changes go to ~/.abhed/config.json (a backup is kept as config.json.bak) and apply to new sessions.\n"+
		"  A model's key: abhed provider login TYPE.")
	return 2
}

// modelsFlags are the flags `abhed models` takes, anywhere among its words.
type modelsFlags struct {
	json, def, check bool
	name             string
	confirmFD        int
	words            []string
}

func parseModelsFlags(args []string) (modelsFlags, error) {
	var f modelsFlags
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			f.words = append(f.words, args[i+1:]...)
			break
		}
		flagName := strings.TrimLeft(a, "-")
		if a == flagName || a == "-" {
			f.words = append(f.words, a)
			continue
		}
		value := func() (string, error) {
			if n, v, ok := strings.Cut(flagName, "="); ok {
				flagName = n
				return v, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("-%s needs a value", flagName)
			}
			i++
			return args[i], nil
		}
		switch strings.SplitN(flagName, "=", 2)[0] {
		case "json":
			f.json = true
		case "default":
			f.def = true
		case "check":
			f.check = true
		case "name":
			v, err := value()
			if err != nil {
				return f, err
			}
			f.name = v
		case "confirm-fd":
			v, err := value()
			if err != nil {
				return f, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 3 {
				return f, fmt.Errorf("-confirm-fd takes a descriptor of 3 or above, not %q", v)
			}
			f.confirmFD = n
		default:
			return f, fmt.Errorf("unknown flag %s", a)
		}
	}
	return f, nil
}

// modelsCmd is `abhed models`.
func modelsCmd(args []string, mio modelsIO) int {
	fail := func(err error) int { fmt.Fprintf(mio.err, "abhed: %v\n", err); return 1 }
	if len(args) == 0 {
		return modelsUsage(mio.err)
	}
	f, err := parseModelsFlags(args[1:])
	if err != nil {
		fmt.Fprintf(mio.err, "abhed: models %s: %v\n", args[0], err)
		return modelsUsage(mio.err)
	}
	switch args[0] {
	case "list", "ls":
		if len(f.words) != 0 {
			return modelsUsage(mio.err)
		}
		return fail0(modelsList(f.json, mio.out), fail)
	case "known":
		if len(f.words) > 1 {
			return modelsUsage(mio.err)
		}
		return fail0(modelsKnown(f.words, f.json, mio.out), fail)
	case "add":
		if len(f.words) < 2 || (f.def && len(f.words) != 2) || (f.name != "" && len(f.words) != 2) {
			return modelsUsage(mio.err)
		}
		res, err := modelsAdd(f.words[0], f.words[1:], f.name)
		if err != nil {
			return fail(err)
		}
		if f.def {
			name := f.name
			if name == "" {
				name = f.words[1]
			}
			d, err := modelsDefault(name, f, mio)
			if err != nil {
				return fail(fmt.Errorf("added, but not made the default: %w", err))
			}
			res.Default = d.Default
		}
		return printModelsResult(res, f.json, mio.out)
	case "default":
		if len(f.words) != 1 {
			return modelsUsage(mio.err)
		}
		res, err := modelsDefault(f.words[0], f, mio)
		if err != nil {
			return fail(err)
		}
		return printModelsResult(res, f.json, mio.out)
	case "remove", "rm":
		if len(f.words) != 1 {
			return modelsUsage(mio.err)
		}
		res, err := modelsRemove(f.words[0])
		if err != nil {
			return fail(err)
		}
		return printModelsResult(res, f.json, mio.out)
	}
	return modelsUsage(mio.err)
}

// modelsResult is what a change did, for -json and for the line printed.
type modelsResult struct {
	File     string   `json:"file"`
	Added    []string `json:"added,omitempty"`
	Existing []string `json:"existing,omitempty"`
	Removed  string   `json:"removed,omitempty"`
	Default  string   `json:"default,omitempty"`
	// Check answers -check: whether the change widens, and how to say it.
	Name        string `json:"name,omitempty"`
	Widens      *bool  `json:"widens,omitempty"`
	Description string `json:"description,omitempty"`
	// Unchanged is set when nothing needed writing.
	Unchanged bool `json:"unchanged,omitempty"`
}

func printModelsResult(r modelsResult, asJSON bool, out io.Writer) int {
	if asJSON {
		b, _ := json.Marshal(r)
		fmt.Fprintln(out, string(b))
		return 0
	}
	file := config.Printable(r.File)
	if len(r.Added) > 0 {
		fmt.Fprintf(out, "added %s to %s; new sessions offer them\n", strings.Join(r.Added, ", "), file)
	}
	if len(r.Existing) > 0 {
		fmt.Fprintf(out, "already configured: %s\n", strings.Join(r.Existing, ", "))
	}
	if r.Removed != "" {
		fmt.Fprintf(out, "removed %s from %s\n", config.Printable(r.Removed), file)
	}
	switch {
	case r.Widens != nil:
		fmt.Fprintln(out, r.Description)
	case r.Default != "" && r.Unchanged:
		fmt.Fprintf(out, "%s is already the default in %s\n", config.Printable(r.Default), file)
	case r.Default != "":
		fmt.Fprintf(out, "new sessions start on %s (model.default in %s)\n", config.Printable(r.Default), file)
	}
	return 0
}

// modelLayers are the person's own file over the defaults, and the managed
// layer on its own; the workspace is never read, as it is never written.
func modelLayers() (own, mgd config.Config, err error) {
	if own, err = userFileConfig(); err != nil {
		return own, mgd, err
	}
	mgd, err = config.LoadManaged()
	return own, mgd, err
}

// userProviders are the providers in the person's own file, by name.
func userProviders(doc map[string]any) map[string]any {
	m, _ := doc["model"].(map[string]any)
	p, _ := m["providers"].(map[string]any)
	return p
}

// modelsAdd adds models to the person's own file. from is a provider type,
// or a configured name whose type, endpoint and key setting are shared.
func modelsAdd(from string, ids []string, name string) (modelsResult, error) {
	var res modelsResult
	if name != "" && !modelName.MatchString(name) {
		return res, fmt.Errorf("%q cannot name a model: letters, digits and . _ : @ - only, up to 64", config.Printable(name))
	}
	for _, id := range ids {
		if !modelID.MatchString(id) {
			return res, fmt.Errorf("%q is not a model id", config.Printable(id))
		}
		if name == "" && !modelName.MatchString(id) {
			return res, fmt.Errorf("%q cannot name a model; give it a name with -name", config.Printable(id))
		}
	}
	unlock, err := lockUserConfig()
	if err != nil {
		return res, err
	}
	defer unlock()
	own, mgd, err := modelLayers()
	if err != nil {
		return res, err
	}
	base, err := providerBase(from, own, mgd)
	if err != nil {
		return res, err
	}
	doc, file, err := readUserDoc()
	if err != nil {
		return res, err
	}
	res.File = file
	providers := userProviders(doc)
	if providers == nil {
		providers = map[string]any{}
	}
	for _, id := range ids {
		entryName := id
		if name != "" {
			entryName = name
		}
		p := base
		p.Model = id
		p.ContextWindow = 0
		if known, window := knownModel(p.Type, id); known && window > 0 {
			p.ContextWindow = window
		}
		if mgd.ManagedSets("model.providers." + entryName) {
			return res, &config.ManagedError{Key: "model.providers." + entryName, Value: entryName, File: managedConfigFile(),
				Reason: "the managed configuration defines that model, so it is changed only there"}
		}
		if err := mgd.EndpointRefused(p); err != nil {
			return res, err
		}
		if have, ok := own.Model.Providers[entryName]; ok {
			if have.Type == p.Type && have.Model == p.Model && have.BaseURL == p.BaseURL {
				res.Existing = append(res.Existing, entryName)
				continue
			}
			return res, fmt.Errorf("a model named %s is already configured (%s, %s); give the new one another name with -name",
				config.Printable(entryName), config.Printable(have.Type), config.Printable(have.Model))
		}
		providers[entryName] = providerEntry(p)
		res.Added = append(res.Added, entryName)
	}
	if len(res.Added) == 0 {
		res.Unchanged = true
		return res, nil
	}
	setDocPath(doc, "model.providers", providers)
	if err := writeUserDoc(file, doc); err != nil {
		return res, err
	}
	for _, n := range res.Added {
		_ = writeConfigChange(configChange{By: "models add", Key: "model.providers", Name: n, Kind: base.Type, File: file})
	}
	return res, nil
}

// providerEntry is p as the file holds it: only what was set, never a key.
func providerEntry(p config.ProviderConfig) map[string]any {
	e := map[string]any{"type": p.Type, "model": p.Model}
	if p.BaseURL != "" {
		e["base_url"] = p.BaseURL
	}
	if p.APIKeyEnv != "" {
		e["api_key_env"] = p.APIKeyEnv
	}
	if p.APIKeySecret != "" {
		e["api_key_secret"] = p.APIKeySecret
	}
	if p.ContextWindow > 0 {
		e["context_window"] = p.ContextWindow
	}
	if p.ProjectID != "" {
		e["project_id"] = p.ProjectID
	}
	if p.SpaceID != "" {
		e["space_id"] = p.SpaceID
	}
	if p.Region != "" {
		e["region"] = p.Region
	}
	if len(p.Extra) > 0 {
		extra := map[string]any{}
		for k, v := range p.Extra {
			extra[k] = v
		}
		e["extra"] = extra
	}
	return e
}

// providerBase is what new models from share: a configured model's type,
// endpoint and key setting (never a key written in the file), or a bare type.
func providerBase(from string, own, mgd config.Config) (config.ProviderConfig, error) {
	for _, c := range []config.Config{mgd, own} {
		if p, ok := c.Model.Providers[from]; ok {
			return config.ProviderConfig{Type: p.Type, BaseURL: p.BaseURL, APIKeyEnv: p.APIKeyEnv, APIKeySecret: p.APIKeySecret,
				ProjectID: p.ProjectID, SpaceID: p.SpaceID, APIVersion: p.APIVersion, IAMURL: p.IAMURL, Region: p.Region, Extra: p.Extra}, nil
		}
	}
	if slices.Contains(model.Providers(), from) {
		if from == "openai-compatible" {
			return config.ProviderConfig{}, errors.New("an openai-compatible model needs its endpoint; add one to ~/.abhed/config.json, then add more with its name")
		}
		return config.ProviderConfig{Type: from}, nil
	}
	return config.ProviderConfig{}, fmt.Errorf("%q is neither a provider type nor a configured model; `abhed providers` lists the types", config.Printable(from))
}

func managedConfigFile() string { return managed.ConfigFile }

// modelsDefault makes name the default in the person's own file. Moving to a
// hosted model sends the code there, so it is confirmed at a terminal or by
// a yes on -confirm-fd; -check only says whether it would be.
func modelsDefault(name string, f modelsFlags, mio modelsIO) (modelsResult, error) {
	res := modelsResult{Name: name}
	own, mgd, err := modelLayers()
	if err != nil {
		return res, err
	}
	if mgd.ManagedSets("model.default") {
		return res, &config.ManagedError{Key: "model.default", Value: name, File: managedConfigFile(),
			Reason: "the managed configuration sets the model, so it is changed only there"}
	}
	p, ok := own.Model.Providers[name]
	if !ok {
		p, ok = mgd.Model.Providers[name]
	}
	if !ok {
		names := make([]string, 0, len(own.Model.Providers))
		for n := range own.Model.Providers {
			names = append(names, n)
		}
		sort.Strings(names)
		return res, fmt.Errorf("no model named %s is configured (%s); add it with `abhed models add`", config.Printable(name), strings.Join(names, ", "))
	}
	if err := mgd.EndpointRefused(p); err != nil {
		return res, err
	}
	_, file, err := readUserDoc()
	if err != nil {
		return res, err
	}
	res.File = file
	old := own.Model.Default
	widens := name != old && providerHosted(p)
	if f.check {
		res.Widens = &widens
		res.Description = fmt.Sprintf("New sessions would start on %s (%s, %s).", config.Printable(name), config.Printable(p.Type), config.Printable(p.Model))
		if widens {
			res.Description += " It is a hosted model: the files and commands a session works with are sent to it."
		}
		return res, nil
	}
	if name == old {
		res.Default, res.Unchanged = name, true
		return res, nil
	}
	if f.confirmFD != 0 {
		fd := os.NewFile(uintptr(f.confirmFD), "confirm") // #nosec G115 -- checked to be 3 or above
		if fd == nil {
			return res, fmt.Errorf("-confirm-fd %d is not open", f.confirmFD)
		}
		defer func() { _ = fd.Close() }()
		mio.in, mio.terminal, mio.byEditor = fd, true, true
	}
	if widens {
		if !mio.terminal {
			return res, errors.New("making a hosted model the default needs a confirmation at a terminal; run it in one")
		}
		fmt.Fprintf(mio.out, "Make %s (%s, %s) the default for new sessions? The files and commands a session works with are sent to it. [y/N] ",
			config.Printable(name), config.Printable(p.Type), config.Printable(p.Model))
		line, _ := bufio.NewReader(mio.in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			if mio.byEditor {
				return res, errors.New("no yes was read from -confirm-fd; the default was not changed")
			}
			return res, errors.New("the default was not changed")
		}
	}
	unlock, err := lockUserConfig()
	if err != nil {
		return res, err
	}
	defer unlock()
	if file, err = writeUserSetting("model.default", name); err != nil {
		return res, err
	}
	by := "models default"
	if mio.byEditor {
		by += " -confirm-fd"
	}
	_ = writeConfigChange(configChange{By: by, Key: "model.default", Name: name, Kind: p.Type, File: file})
	res.Default = name
	return res, nil
}

// modelsRemove takes a model out of the person's own file. It narrows, so it
// does not ask; the default is never removed from under new sessions.
func modelsRemove(name string) (modelsResult, error) {
	var res modelsResult
	unlock, err := lockUserConfig()
	if err != nil {
		return res, err
	}
	defer unlock()
	doc, file, err := readUserDoc()
	if err != nil {
		return res, err
	}
	res.File = file
	providers := userProviders(doc)
	if _, ok := providers[name]; !ok {
		return res, fmt.Errorf("%s has no model named %s", config.Printable(file), config.Printable(name))
	}
	if m, _ := doc["model"].(map[string]any); m != nil && m["default"] == name {
		return res, fmt.Errorf("%s is the default; make another model the default first (abhed models default NAME)", config.Printable(name))
	}
	delete(providers, name)
	if err := writeUserDoc(file, doc); err != nil {
		return res, err
	}
	_ = writeConfigChange(configChange{By: "models remove", Key: "model.providers", Name: name, File: file})
	res.Removed = name
	return res, nil
}

// modelRow is one model in `abhed models list`; never its key or endpoint.
type modelRow struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Model   string `json:"model"`
	Default bool   `json:"default,omitempty"`
	Hosted  bool   `json:"hosted"`
	// Key is the key's name and Source where it is found now.
	Key    string `json:"key,omitempty"`
	Source string `json:"source,omitempty"`
}

func modelsList(asJSON bool, out io.Writer) error {
	own, mgd, err := modelLayers()
	if err != nil {
		return err
	}
	all := map[string]config.ProviderConfig{}
	for n, p := range own.Model.Providers {
		all[n] = p
	}
	for n, p := range mgd.Model.Providers {
		all[n] = p
	}
	def := own.Model.Default
	if mgd.ManagedSets("model.default") {
		def = mgd.Model.Default
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	rows := make([]modelRow, 0, len(names))
	for _, n := range names {
		p := config.ResolveKey(all[n])
		r := modelRow{Name: n, Type: p.Type, Model: p.Model, Default: n == def, Hosted: providerHosted(p), Key: p.KeySource.Name, Source: p.KeySource.Kind}
		if r.Key != "" && r.Source == "" {
			r.Source = "none"
		}
		rows = append(rows, r)
	}
	if asJSON {
		b, _ := json.Marshal(map[string]any{"models": rows})
		fmt.Fprintln(out, string(b))
		return nil
	}
	for _, r := range rows {
		mark := " "
		if r.Default {
			mark = "*"
		}
		key := ""
		if r.Key != "" {
			key = "  key " + r.Key + ": " + sourceWord(r.Source)
		}
		fmt.Fprintf(out, "%s %-28s %-12s %s%s\n", mark, config.Printable(r.Name), config.Printable(r.Type), config.Printable(r.Model), key)
	}
	return nil
}

// sourceWord says a key's source in a few words.
func sourceWord(kind string) string {
	switch kind {
	case modelkeys.KindEnv:
		return "from the environment"
	case modelkeys.KindKeychain:
		return "stored in the Keychain"
	case modelkeys.KindFile:
		return "stored in Abhed's secret store"
	case modelkeys.KindConfig:
		return "in the configuration"
	}
	return "not set"
}

// knownType is one type in `abhed models known`.
type knownType struct {
	Type   string       `json:"type"`
	Hosted bool         `json:"hosted"`
	Key    string       `json:"key,omitempty"`
	Models []knownEntry `json:"models"`
}

type knownEntry struct {
	ID            string `json:"id"`
	ContextWindow int    `json:"context_window,omitempty"`
}

func modelsKnown(words []string, asJSON bool, out io.Writer) error {
	types := model.Providers()
	if len(words) == 1 {
		if !slices.Contains(types, words[0]) {
			return fmt.Errorf("%q is not a provider type; `abhed providers` lists them", config.Printable(words[0]))
		}
		types = words
	}
	var list []knownType
	for _, t := range types {
		if t == "openai-compatible" {
			continue
		}
		k := knownType{Type: t, Hosted: !slices.Contains(localTypes, t), Models: []knownEntry{}}
		if h, ok := modelkeys.HostedType(t); ok {
			k.Key = h.Key
		}
		for _, id := range knownModels(t) {
			_, w := knownModel(t, id)
			k.Models = append(k.Models, knownEntry{ID: id, ContextWindow: w})
		}
		list = append(list, k)
	}
	if asJSON {
		b, _ := json.Marshal(map[string]any{"types": list})
		fmt.Fprintln(out, string(b))
		return nil
	}
	for _, k := range list {
		ids := make([]string, 0, len(k.Models))
		for _, m := range k.Models {
			ids = append(ids, m.ID)
		}
		line := "(any model id the endpoint serves)"
		if len(ids) > 0 {
			line = strings.Join(ids, ", ")
		}
		fmt.Fprintf(out, "%-18s %s\n", k.Type, line)
	}
	return nil
}
