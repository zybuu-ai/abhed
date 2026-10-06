package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// The web sections are managed only. web_search sends the agent's queries to
// a provider and web_fetch reaches the hosts it names, so only the managed
// configuration turns them on or says where they go. Every other layer, the
// user's file, -settings, a workspace trusted or not, may turn them off or
// narrow them, and what it may not do is set aside and recorded.

// webReason is why a lower layer's web setting was set aside.
const webReason = "only the managed configuration turns web search or web fetch on or says where they go; " +
	"another layer may only turn them off or narrow them (`sudo abhed admin web-search on`)"

// WebKey reports whether path is in the web_search or web_fetch section.
func WebKey(path string) bool {
	for _, s := range []string{"web_search", "web_fetch"} {
		if path == s || strings.HasPrefix(path, s+".") {
			return true
		}
	}
	return false
}

// webIntent is a lower layer's web setting that may narrow what the managed
// configuration allows. It is applied after the managed file, so a managed
// "on" cannot undo it.
type webIntent struct {
	key, file, layer string
	off              bool
	limit            int
	hosts            []string
}

// setAsideWeb takes the web settings the layer just merged out of c: the ones
// that narrow are kept as intents for after the managed file, every other one
// is set aside. The sections go back to their defaults either way.
func setAsideWeb(c *Config, file, layer string) {
	d := Default()
	kept := c.SetKeys[:0:0]
	for _, k := range c.SetKeys {
		if !WebKey(k) {
			kept = append(kept, k)
			continue
		}
		in := webIntent{key: k, file: file, layer: layer}
		switch k {
		case "web_search.enabled":
			in.off = !c.WebSearch.Enabled
		case "web_fetch.enabled":
			in.off = !c.WebFetch.Enabled
		case "web_search.max_results":
			in.limit = c.WebSearch.MaxResults
		case "web_fetch.max_chars":
			in.limit = c.WebFetch.MaxChars
		case "web_fetch.allowed_hosts":
			in.hosts = slices.Clone(c.WebFetch.AllowedHosts)
		}
		if in.off || in.limit > 0 || len(in.hosts) > 0 {
			c.webIntents = append(c.webIntents, in)
			continue
		}
		// A starter file repeats the defaults; saying so again asks for nothing.
		if webValue(c, k) == webValue(&d, k) {
			continue
		}
		c.SetAside = append(c.SetAside, SetAsideKey{File: file, Layer: layer, Key: k,
			Value: webValue(c, k), Reason: webReason})
	}
	c.SetKeys = kept
	c.WebSearch, c.WebFetch = d.WebSearch, d.WebFetch
}

// webValue is the value c holds at a web key, as a set-aside names it:
// a credential is never shown, and a URL loses its password.
func webValue(c *Config, k string) string {
	var v any
	switch k {
	case "web_search.enabled":
		v = c.WebSearch.Enabled
	case "web_fetch.enabled":
		v = c.WebFetch.Enabled
	case "web_search.provider":
		v = c.WebSearch.Provider
	case "web_search.base_url":
		v = c.WebSearch.BaseURL
	case "web_search.api_key":
		if c.WebSearch.APIKey == "" {
			return `""`
		}
		return "[redacted]"
	case "web_search.api_key_env":
		v = c.WebSearch.APIKeyEnv
	case "web_search.max_results":
		v = c.WebSearch.MaxResults
	case "web_fetch.max_chars":
		v = c.WebFetch.MaxChars
	case "web_fetch.allowed_hosts":
		v = c.WebFetch.AllowedHosts
	default:
		return ""
	}
	return shortJSON(redact(k, v))
}

// applyWebIntents applies, over the managed file, what the lower layers asked
// that narrows the web sections, and sets aside what would not narrow.
func applyWebIntents(c *Config) {
	for _, in := range c.webIntents {
		narrowed := func(value, how string) {
			c.Narrowed = append(c.Narrowed, SetAsideKey{File: in.file, Layer: in.layer, Key: in.key, Value: value, Reason: how})
			if !slices.Contains(c.SetKeys, in.key) {
				c.SetKeys = append(c.SetKeys, in.key)
			}
		}
		switch in.key {
		case "web_search.enabled":
			if c.WebSearch.Enabled {
				c.WebSearch.Enabled = false
				narrowed("false", "turned web search off, which a layer below the managed configuration may do")
			}
		case "web_fetch.enabled":
			if c.WebFetch.Enabled {
				c.WebFetch.Enabled = false
				narrowed("false", "turned web fetch off, which a layer below the managed configuration may do")
			}
		case "web_search.max_results":
			if cur := orInt(c.WebSearch.MaxResults, 5); in.limit < cur {
				c.WebSearch.MaxResults = in.limit
				narrowed(fmt.Sprint(in.limit), fmt.Sprintf("lowered the results per search from %d", cur))
			}
		case "web_fetch.max_chars":
			if cur := orInt(c.WebFetch.MaxChars, 20000); in.limit < cur {
				c.WebFetch.MaxChars = in.limit
				narrowed(fmt.Sprint(in.limit), fmt.Sprintf("lowered the characters per fetch from %d", cur))
			}
		case "web_fetch.allowed_hosts":
			narrowHosts(c, in, narrowed)
		}
	}
	c.webIntents = nil
}

// narrowHosts keeps the hosts of a lower layer's list that the managed list
// already covers. A list where the managed file names none would let those
// hosts run unasked, so it is set aside; a list covering none of the managed
// hosts allows no host, and turns web fetch off.
func narrowHosts(c *Config, in webIntent, narrowed func(value, how string)) {
	aside := func(h, why string) {
		c.SetAside = append(c.SetAside, SetAsideKey{File: in.file, Layer: in.layer, Key: in.key, Value: h, Reason: why})
	}
	managed := c.WebFetch.AllowedHosts
	if len(managed) == 0 || !c.ManagedSets("web_fetch.allowed_hosts") {
		for _, h := range in.hosts {
			aside(h, "the managed configuration names no hosts for web fetch, so a host listed here would be fetched unasked")
		}
		return
	}
	var kept []string
	for _, h := range in.hosts {
		if hostCovered(h, managed) {
			kept = append(kept, h)
			continue
		}
		aside(h, "the managed configuration's web_fetch.allowed_hosts does not include it")
	}
	if len(kept) == 0 {
		if c.WebFetch.Enabled {
			c.WebFetch.Enabled = false
			narrowed("false", "listed no host the managed configuration allows, so web fetch is off")
		}
		return
	}
	c.WebFetch.AllowedHosts = kept
	narrowed(shortJSON(kept), "narrowed the hosts web fetch may reach")
}

// hostCovered reports whether pattern h allows nothing that list does not.
func hostCovered(h string, list []string) bool {
	h = strings.ToLower(h)
	for _, m := range list {
		m = strings.ToLower(m)
		if h == m {
			return true
		}
		if dom, ok := strings.CutPrefix(m, "*."); ok {
			// *.a.example and b.a.example are both inside *.example.
			if strings.HasSuffix(strings.TrimPrefix(h, "*."), "."+dom) {
				return true
			}
		}
	}
	return false
}

func orInt(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}

// origin is where a lower layer's setting came from, for an override.
type origin struct {
	file, layer string
	value       any
}

// noteOrigins remembers each setting data makes, so a managed value that
// replaces it is recorded rather than silent.
func noteOrigins(c *Config, file, layer string, data []byte) {
	var raw any
	if json.Unmarshal(data, &raw) != nil {
		return
	}
	walkSet("", raw, reflect.TypeFor[Config](), func(p string, v any) { noteOrigin(c, file, layer, p, v) })
}

func noteOrigin(c *Config, file, layer, path string, v any) {
	if ManagedOnly(path) || WebKey(path) {
		return // set aside on its own, and recorded so
	}
	if c.origins == nil {
		c.origins = map[string]origin{}
	}
	c.origins[path] = origin{file: file, layer: layer, value: v}
}

// noteOverrides records each lower layer's setting the managed file data
// replaced with another value.
func noteOverrides(c *Config, data []byte) {
	if len(c.origins) == 0 {
		return
	}
	var raw any
	if json.Unmarshal(data, &raw) != nil {
		return
	}
	walkSet("", raw, reflect.TypeFor[Config](), func(p string, v any) {
		o, ok := c.origins[p]
		if !ok || sameJSON(o.value, v) {
			return
		}
		c.Overridden = append(c.Overridden, SetAsideKey{File: o.file, Layer: o.layer, Key: p,
			Value:  shortJSON(redact(p, o.value)),
			Reason: "the managed configuration sets it to " + shortJSON(redact(p, v))})
	})
	c.origins = nil
}

func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// WebSearchState says who decides web search and what it is now, for doctor
// and the surfaces that describe a session.
func (c Config) WebSearchState() string {
	switch {
	case c.WebSearch.Enabled:
		return "enabled by the managed configuration (" + orStr(c.WebSearch.Provider, "duckduckgo") + ")"
	case c.ManagedSets("web_search.enabled") && c.narrowedOff("web_search.enabled"):
		return "off: the managed configuration enables it and " + c.narrowedBy("web_search.enabled") + " turns it off"
	}
	return "off; only the managed configuration can enable it"
}

// WebFetchState is WebSearchState for web fetch.
func (c Config) WebFetchState() string {
	switch {
	case c.WebFetch.Enabled && len(c.WebFetch.AllowedHosts) > 0:
		return "enabled by the managed configuration for " + strings.Join(c.WebFetch.AllowedHosts, ", ")
	case c.WebFetch.Enabled:
		return "enabled by the managed configuration (any public page, asked first)"
	case c.ManagedSets("web_fetch.enabled") && c.narrowedOff("web_fetch.enabled"):
		return "off: the managed configuration enables it and " + c.narrowedBy("web_fetch.enabled") + " turns it off"
	}
	return "off; only the managed configuration can enable it"
}

func (c Config) narrowedOff(key string) bool { return c.narrowedBy(key) != "" }

func (c Config) narrowedBy(key string) string {
	for _, n := range c.Narrowed {
		if n.Key == key || (key == "web_fetch.enabled" && n.Key == "web_fetch.allowed_hosts" && n.Value == "false") {
			return Printable(n.File)
		}
	}
	return ""
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Attempt is one setting a layer made, or a change someone asked for, that
// did not take effect as written: what the record keeps of it.
type Attempt struct {
	Layer string
	// Source is the file, flag or variable that made the setting.
	Source string
	Key    string
	// Value is the value asked for, with credentials redacted.
	Value string
	// Decision is set_aside, ignored_untrusted, overridden or narrowed.
	Decision string
	Reason   string
}

// Attempts are the settings loading did not take as written: every
// set-aside, every lower value the managed file replaced, every narrowing of
// the web sections, and the web settings an untrusted workspace made.
func (c Config) Attempts() []Attempt {
	var out []Attempt
	add := func(ks []SetAsideKey, decision string) {
		for _, k := range ks {
			layer := k.Layer
			if layer == "" {
				layer = c.layerOf(k.File)
			}
			out = append(out, Attempt{Layer: layer, Source: k.File, Key: k.Key, Value: k.Value, Decision: decision, Reason: k.Reason})
		}
	}
	add(c.SetAside, "set_aside")
	add(c.Overridden, "overridden")
	add(c.Narrowed, "narrowed")
	for _, k := range c.Workspace.Ignored {
		if WebKey(k.Key) {
			out = append(out, Attempt{Layer: LayerWorkspace, Source: c.Workspace.File, Key: k.Key, Value: k.Value,
				Decision: "ignored_untrusted", Reason: "the workspace configuration is not trusted, and " + webReason})
		}
	}
	if a, ok := c.usersAttempt(); ok {
		out = append(out, a)
	}
	return out
}

func (c Config) layerOf(file string) string {
	switch file {
	case "":
		return ""
	case c.Workspace.File:
		return LayerWorkspace
	case c.Settings.Name:
		return LayerSettings
	}
	return LayerUser
}

// RedactValue is v as a record shows a value set at key: credentials
// redacted, a URL without its password, and cut to a short line.
func RedactValue(key string, v any) string { return shortJSON(redact(key, v)) }
