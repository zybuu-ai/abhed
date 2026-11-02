package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/orgpolicy"
)

// OrgPolicyConfig is the machine's say over the signed org policy. Managed
// only: the managed file sets it, never the org policy itself.
type OrgPolicyConfig struct {
	// URL, when set, is the only org server whose policy this machine takes.
	URL string `json:"url,omitempty"`
	// Keys, when set, are the only signing keys trusted (base64 Ed25519 or
	// PEM); a statement cannot rotate them.
	Keys []string `json:"keys,omitempty"`
	// Required applies the fallback when no verified, fresh policy is held.
	Required bool `json:"required,omitempty"`
	// MaxAgeHours caps how long a policy applies without a fresh one.
	MaxAgeHours int `json:"max_age_hours,omitempty"`
	// RefreshMinutes is how often a running Abhed fetches it again (15).
	RefreshMinutes int `json:"refresh_minutes,omitempty"`
}

// Machine is the settings as package orgpolicy takes them.
func (o OrgPolicyConfig) Machine() orgpolicy.Machine {
	return orgpolicy.Machine{URL: strings.TrimRight(o.URL, "/"), Keys: o.Keys, Required: o.Required,
		MaxAge: time.Duration(o.MaxAgeHours) * time.Hour}
}

// Refresh is how often a running Abhed fetches the policy again.
func (o OrgPolicyConfig) Refresh() time.Duration {
	if o.RefreshMinutes > 0 {
		return time.Duration(o.RefreshMinutes) * time.Minute
	}
	return 15 * time.Minute
}

// OrgPolicyStatus is what loading decided about the org policy.
type OrgPolicyStatus struct {
	// Status is none, applied or fallback.
	Status string
	Reason string
	URL    string
	// Profile is the login profile the policy is fetched with.
	Profile string
	Name    string
	Version int64
	Epoch   int64
	Digest  string
	KeyID   string
	Issued  time.Time
	// Keys are the settings the org policy made that took effect, and
	// Shadowed those the machine's managed file set as well, which won.
	Keys     []string
	Shadowed []string
}

// LayerOrg is the signed org policy, below the machine's managed file.
const LayerOrg = "org"

// orgSource names the org policy in messages.
func orgSource(url string) string {
	if url == "" {
		return "the org policy"
	}
	return "the org policy from " + url
}

// orgRefused are the settings an org policy may not make: programs, local
// files or environment variables on the machine, or a server's configuration.
var orgRefused = map[string]string{
	"org_policy":                    "only the machine's managed file says where the org policy comes from",
	"additional_dirs":               "an org policy does not name folders on this machine",
	"extensions":                    "an org policy does not name programs to run on this machine",
	"mcp":                           "an org policy does not name MCP servers to run or reach from this machine",
	"statusline":                    "an org policy does not name programs to run on this machine",
	"commands.dirs":                 "an org policy does not name folders on this machine",
	"rules.dirs":                    "an org policy does not name folders on this machine",
	"skills.dirs":                   "an org policy does not name folders on this machine",
	"agents.dirs":                   "an org policy does not name folders on this machine",
	"context.memory_files":          "an org policy does not name files on this machine",
	"k8s":                           "an org policy does not name this machine's clusters or their files",
	"storage":                       "an org policy does not configure storage",
	"server":                        "an org policy does not configure a server",
	"auth":                          "an org policy does not configure a server's sign-in",
	"schedules":                     "an org policy does not configure a server's schedules",
	"control_plane":                 "an org policy does not configure a server's control plane",
	"record.dir":                    "an org policy does not name folders on this machine",
	"record.ocsf.path":              "an org policy does not name files on this machine",
	"record.witness.file":           "an org policy does not name files on this machine",
	"sandbox.microvm_socket":        "an org policy does not name files on this machine",
	"model.providers.*.api_key_env": "an org policy does not read this machine's environment",
}

// orgRefusedOf is the rule path falls under, if any.
func orgRefusedOf(path string) (string, bool) {
	segs := strings.Split(path, ".")
	for rule, why := range orgRefused {
		rs := strings.Split(rule, ".")
		if len(rs) > len(segs) {
			continue
		}
		ok := true
		for i, r := range rs {
			if r != "*" && r != segs[i] {
				ok = false
				break
			}
		}
		if ok {
			return why, true
		}
	}
	return "", false
}

// orgSetAside is a setting the org policy made that was left out.
type orgSetAside struct{ key, why string }

// stripOrg removes the settings an org policy may not make from doc.
func stripOrg(doc map[string]any) []orgSetAside {
	var out []orgSetAside
	var walk func(path string, obj map[string]any, t reflect.Type)
	walk = func(path string, obj map[string]any, t reflect.Type) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		isStruct := t.Kind() == reflect.Struct && !reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]())
		var fields map[string]reflect.StructField
		if isStruct {
			fields = jsonFields(t)
		}
		for _, k := range slices.Sorted(maps.Keys(obj)) {
			var p string
			var ft reflect.Type
			switch {
			case isStruct:
				f, ok := fieldFor(fields, k)
				if !ok || annotation(k) {
					continue
				}
				p, ft = join(path, jsonName(f)), f.Type
			case t.Kind() == reflect.Map:
				p, ft = join(path, k), t.Elem()
			default:
				continue
			}
			if why, refused := orgRefusedOf(p); refused {
				delete(obj, k)
				out = append(out, orgSetAside{key: p, why: why})
				continue
			}
			if sub, ok := obj[k].(map[string]any); ok {
				walk(p, sub, ft)
			}
		}
	}
	walk("", doc, reflect.TypeFor[Config]())
	return out
}

// unionLists are the managed settings both sources' entries are kept of: a
// deny or ask rule is only ever added.
var unionLists = map[string]bool{"permissions.deny": true, "permissions.ask": true}

// mergeDocs lays local over org setting by setting: local wins, map entries
// are replaced whole, and deny and ask rules are joined.
func mergeDocs(org, local any, t reflect.Type, path string) any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	oo, ook := org.(map[string]any)
	lo, lok := local.(map[string]any)
	switch {
	case ook && lok && t.Kind() == reflect.Struct && !reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]()):
		fields := jsonFields(t)
		out := make(map[string]any, len(oo)+len(lo))
		byFold := map[string]string{}
		for k, v := range oo {
			out[k] = v
			byFold[foldKey(k)] = k
		}
		for k, v := range lo {
			ok, seen := byFold[foldKey(k)]
			if !seen {
				out[k] = v
				continue
			}
			delete(out, ok)
			if f, found := fieldFor(fields, k); found && !annotation(k) {
				out[k] = mergeDocs(oo[ok], v, f.Type, join(path, jsonName(f)))
			} else {
				out[k] = v
			}
		}
		return out
	case ook && lok && t.Kind() == reflect.Map:
		out := maps.Clone(oo)
		maps.Copy(out, lo)
		return out
	}
	ol, oListed := org.([]any)
	ll, lListed := local.([]any)
	if unionLists[path] && oListed && lListed {
		out := slices.Clone(ll)
		for _, r := range ol {
			if !slices.ContainsFunc(out, func(x any) bool { return sameJSON(x, r) }) {
				out = append(out, r)
			}
		}
		return out
	}
	return local
}

// decodeDoc reads a managed document keeping numbers as written.
func decodeDoc(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("it is not a JSON object")
	}
	return m, nil
}

// CheckOrgPolicy reports whether doc loads as an org policy: an object
// with no key written twice, that the configuration reads and accepts.
func CheckOrgPolicy(doc []byte) error {
	if err := CheckManagedJSON(doc); err != nil {
		return err
	}
	m, err := decodeDoc(doc)
	if err != nil {
		return err
	}
	stripOrg(m)
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	cfg := Default()
	if _, err := mergeData(&cfg, "org policy", b); err != nil {
		return err
	}
	cfg.ManagedKeys = managedKeys(b)
	cfg.Managed = true
	return cfg.Validate()
}

// orgMachine is the org_policy section of the machine's managed file.
func orgMachine(local []byte) OrgPolicyConfig {
	var m struct {
		OrgPolicy OrgPolicyConfig `json:"org_policy"`
	}
	if local != nil {
		_ = json.Unmarshal(local, &m)
	}
	return m.OrgPolicy
}

// orgEffective reads the org policy state and decides what applies.
var orgEffective = func(m orgpolicy.Machine) orgpolicy.Effective {
	path, err := orgpolicy.Path()
	if err != nil {
		return orgpolicy.Evaluate(nil, m, time.Now())
	}
	st, err := orgpolicy.LoadState(path)
	if err != nil {
		// A state that cannot be read is not trusted, and not taken as absent.
		e := orgpolicy.Evaluate(nil, orgpolicy.Machine{URL: m.URL, Required: true}, time.Now())
		e.Reason = "the org policy state cannot be read: " + err.Error()
		return e
	}
	return orgpolicy.Evaluate(st, m, time.Now())
}

// orgLayer works out the org policy document to lay under the machine's
// managed file (local, nil when there is none) and notes its status.
func orgLayer(cfg *Config, local []byte) (map[string]any, string) {
	mc := orgMachine(local)
	e := orgEffective(mc.Machine())
	st := OrgPolicyStatus{Status: e.Status, Reason: e.Reason, URL: e.URL, Profile: e.Profile, KeyID: e.KeyID}
	if e.Statement != nil {
		st.Name, st.Version, st.Epoch, st.Digest, st.Issued = e.Statement.Name, e.Statement.Version, e.Statement.Epoch, e.Statement.Digest(), e.Statement.IssuedAt
	}
	defer func() { cfg.OrgPolicy = st }()
	if e.Status == orgpolicy.StatusNone {
		return nil, ""
	}
	source := orgSource(e.URL)
	var doc map[string]any
	if e.Doc != nil {
		var err error
		if err = CheckManagedJSON(e.Doc); err == nil {
			doc, err = decodeDoc(e.Doc)
		}
		if err != nil {
			doc, e.Fallback = nil, true
			st.Status, st.Reason = orgpolicy.StatusFallback, "the org policy does not read: "+err.Error()
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}
	for _, s := range stripOrg(doc) {
		cfg.SetAside = append(cfg.SetAside, SetAsideKey{File: source, Layer: LayerOrg, Key: s.key, Reason: s.why})
	}
	if e.Fallback {
		fb, _ := decodeDoc(orgpolicy.Fallback())
		doc, _ = mergeDocs(doc, fb, reflect.TypeFor[Config](), "").(map[string]any)
		source = "the org policy fallback"
	}
	return doc, source
}

// orgKeysOf notes which of the org document's settings took effect and
// which the machine's file shadowed.
func orgKeysOf(cfg *Config, org map[string]any, local []byte) {
	b, err := json.Marshal(org)
	if err != nil {
		return
	}
	mine := managedKeys(local)
	for _, k := range managedKeys(b) {
		shadowed := false
		for _, l := range mine {
			if !unionLists[k] && (l == k || strings.HasPrefix(k, l+".") || strings.HasPrefix(l, k+".")) {
				shadowed = true
				break
			}
		}
		if shadowed {
			cfg.OrgPolicy.Shadowed = append(cfg.OrgPolicy.Shadowed, k)
		} else {
			cfg.OrgPolicy.Keys = append(cfg.OrgPolicy.Keys, k)
		}
	}
	sort.Strings(cfg.OrgPolicy.Keys)
	sort.Strings(cfg.OrgPolicy.Shadowed)
}

// FromOrg reports whether the org policy, not the machine's managed file,
// made the managed setting at path.
func (c Config) FromOrg(path string) bool {
	for _, k := range c.OrgPolicy.Keys {
		if k == path || strings.HasPrefix(k, path+".") || strings.HasPrefix(path, k+".") {
			return true
		}
	}
	return false
}

// managedSourceOf names where a managed setting came from, for a refusal.
func (c Config) managedSourceOf(key string) string {
	if c.FromOrg(key) {
		if c.OrgPolicy.Status == orgpolicy.StatusFallback {
			return "the org policy fallback"
		}
		return orgSource(c.OrgPolicy.URL)
	}
	return ""
}

// OrgPolicyLine is one line on what applies, for status output.
func (s OrgPolicyStatus) OrgPolicyLine() string {
	switch s.Status {
	case orgpolicy.StatusApplied:
		return fmt.Sprintf("%s %q version %d from %s, signed %s by %s", s.Status, s.Name, s.Version, s.URL,
			s.Issued.Local().Format("2 Jan 15:04"), s.KeyID)
	case orgpolicy.StatusFallback:
		return "fallback (plan mode, no allow rules, no web tools): " + s.Reason
	}
	return "none: " + s.Reason
}

// warnOrgFallback says the fallback applies, and why.
func warnOrgFallback(w warnSink, c Config) {
	if c.OrgPolicy.Status == orgpolicy.StatusFallback {
		w.emit("orgpolicy\x00fallback", "the org policy fallback applies (plan mode, no allow rules, no web tools): "+c.OrgPolicy.Reason)
	}
}

// MachineOrgPolicy is the org_policy section of the machine's managed file
// alone, as `abhed login` and the policy client read it.
func MachineOrgPolicy() (OrgPolicyConfig, error) {
	local, err := readManagedFile(managed.ConfigFile)
	if err != nil || local == nil {
		return OrgPolicyConfig{}, err
	}
	if err := CheckManagedJSON(local); err != nil {
		return OrgPolicyConfig{}, fmt.Errorf("managed configuration %s: %w", managed.ConfigFile, err)
	}
	return orgMachine(local), nil
}

func typeOfConfig() reflect.Type { return reflect.TypeFor[Config]() }
