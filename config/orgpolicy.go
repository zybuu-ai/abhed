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

// orgKind is what an org policy may do with a setting.
type orgKind int

const (
	// orgRefuse: never, pinned keys or not.
	orgRefuse orgKind = iota
	// orgTighten: only where it tightens what is below, by the setting's
	// workspace-trust rule; loosening needs machine-pinned keys.
	orgTighten
	// orgPinned: only when the machine's managed file pins org_policy.keys.
	orgPinned
	// orgExtra: always, as its check allows.
	orgExtra
)

// orgRule classifies one setting, or a section, for the org policy.
type orgRule struct {
	kind orgKind
	why  string
}

// orgRules classify every setting for the org policy, by the nearest path.
// Anything naming a program, a local file, an environment variable or an
// endpoint is refused; TestEveryConfigFieldIsClassifiedForTheOrg holds it whole.
var orgRules = map[string]orgRule{
	"permissions.deny":           {orgTighten, ""},
	"permissions.ask":            {orgTighten, ""},
	"permissions.mode":           {orgTighten, ""},
	"permissions.allow":          {orgPinned, "an allow rule widens what runs unasked"},
	"permissions.git_extensions": {orgPinned, "a git extension opted in runs unasked"},

	"sandbox.min_tier":               {orgTighten, ""},
	"sandbox.allow_network":          {orgTighten, ""},
	"sandbox.max_memory_mb":          {orgTighten, ""},
	"sandbox.max_procs":              {orgTighten, ""},
	"sandbox.readable_home":          {orgTighten, ""},
	"sandbox.shared_caches":          {orgTighten, ""},
	"sandbox.max_cpu_percent":        {orgTighten, ""},
	"sandbox.terminal":               {orgTighten, ""},
	"sandbox.terminal_idle_minutes":  {orgTighten, ""},
	"sandbox.toolchains":             {orgTighten, ""},
	"sandbox.toolchains_registries":  {orgTighten, ""},
	"sandbox.listen":                 {orgTighten, ""},
	"sandbox.tier":                   {orgPinned, "chooses what confines commands"},
	"sandbox.network":                {orgPinned, "turns on the egress allowlist"},
	"sandbox.protect_git":            {orgPinned, "turned off, commands can plant a git hook"},
	"sandbox.listen_ports":           {orgPinned, "names the ports commands may listen on"},
	"sandbox.toolchains_preapproved": {orgPinned, "an install that needs no ask"},
	"sandbox.read_only_paths":        {orgRefuse, "names folders on this machine"},
	"sandbox.language_servers":       {orgRefuse, "names programs to run on this machine"},
	"sandbox.image":                  {orgRefuse, "chooses the image commands run in"},
	"sandbox.microvm_socket":         {orgRefuse, "names a file on this machine"},
	"fence.cpu_percent":              {orgPinned, "tunes the fence tier's limits"},
	"fence.socketcall":               {orgPinned, "lets commands make sockets through socketcall"},
	"fence.egress_port_only":         {orgPinned, "lets the fence hold the allowlist by port alone"},
	"fence.allow_root":               {orgPinned, "lets the fence run as root"},
	"egress.rules":                   {orgPinned, "names the destinations commands reach"},
	"egress.default":                 {orgPinned, "sets what commands reach by default"},
	"egress.mode":                    {orgPinned, "sets the egress mode"},
	"egress.record_paths":            {orgPinned, "sets what the egress record keeps"},
	"egress.idle_seconds":            {orgPinned, "sets how long an idle connection lasts"},
	"egress.inspect_tls":             {orgPinned, "has the egress proxy look inside HTTPS"},

	"limits.max_turns":                {orgTighten, ""},
	"limits.max_tokens":               {orgTighten, ""},
	"limits.max_budget_tokens":        {orgTighten, ""},
	"limits.max_subagents":            {orgTighten, ""},
	"limits.max_parallel_subagents":   {orgTighten, ""},
	"limits.nested_subagents":         {orgTighten, ""},
	"limits.max_background_subagents": {orgTighten, ""},
	"limits.background_shells":        {orgTighten, ""},
	"limits.background_max_minutes":   {orgTighten, ""},
	"subagents.wake":                  {orgTighten, ""},
	"subagents.max_wakes_per_hour":    {orgTighten, ""},
	"subagents.wake_max_turns":        {orgTighten, ""},

	"tools.syntax_check":          {orgTighten, ""},
	"tools.bash_checkpoints":      {orgTighten, ""},
	"tools.bash_checkpoint_mb":    {orgPinned, "sets how much of the workspace is held in memory"},
	"tools.bash_checkpoint_files": {orgPinned, "sets how much of the workspace is held in memory"},

	"web_search.enabled":     {orgTighten, ""},
	"web_search.max_results": {orgTighten, ""},
	"web_search":             {orgRefuse, "a search provider, key and endpoint receive the agent's queries"},
	"web_fetch.enabled":      {orgTighten, ""},
	"web_fetch.max_chars":    {orgTighten, ""},
	"web_fetch":              {orgRefuse, "names hosts fetched without asking"},
	"k8s.enabled":            {orgTighten, ""},
	"k8s.allow_writes":       {orgTighten, ""},
	"k8s":                    {orgRefuse, "names this machine's clusters and their files"},
	"ssh.enabled":            {orgTighten, ""},
	"ssh.transfers":          {orgTighten, ""},
	"ssh.transfer_max_mb":    {orgTighten, ""},
	"ssh.hosts":              {orgRefuse, "names machines, keys, files and environment variables"},
	"ssh.connect_hosts":      {orgRefuse, "names machines the agent may reach"},
	"ssh.read_commands":      {orgRefuse, "lets commands run on a host without asking"},
	"skills.disabled":        {orgTighten, ""},
	"skills.dirs":            {orgRefuse, "names folders on this machine"},
	"agents.disabled":        {orgTighten, ""},
	"agents.dirs":            {orgRefuse, "names folders on this machine"},
	"telemetry":              {orgRefuse, "names an endpoint and headers that receive the event stream"},
	"network":                {orgRefuse, "names this machine's proxy, its files and secrets, and carries the policy's own fetch"},

	"commands.dirs":                {orgRefuse, "names folders on this machine"},
	"rules.dirs":                   {orgRefuse, "names folders on this machine"},
	"statusline":                   {orgRefuse, "names a program to run on this machine"},
	"memory.auto":                  {orgTighten, ""},
	"memory.import_depth":          {orgTighten, ""},
	"cli":                          {orgRefuse, "the machine's managed file sets it"},
	"record.ship.enabled":          {orgExtra, ""},
	"record.ship.max_backlog_mb":   {orgExtra, ""},
	"record":                       {orgRefuse, "names files on this machine, or erases the record"},
	"org_policy":                   {orgRefuse, "only the machine's managed file says where the org policy comes from"},
	"hooks.managed_only":           {orgExtra, ""},
	"hooks.disabled":               {orgPinned, "switching hooks off removes their vetoes"},
	"studio.disable_host_terminal": {orgExtra, ""},
	"studio":                       {orgRefuse, "the machine's managed file sets it"},
	"suggest.enabled":              {orgTighten, ""},
	"suggest.model":                {orgRefuse, "a provider receives the conversation"},
	"plan.runbooks":                {orgTighten, ""},
	"mode.default":                 {orgTighten, ""},

	"monitor.enabled":        {orgTighten, ""},
	"monitor.on_unavailable": {orgTighten, ""},
	"monitor.max_per_turn":   {orgTighten, ""},
	"monitor.honeytokens":    {orgTighten, ""},
	"monitor":                {orgRefuse, "a judge endpoint and key receive the conversation"},

	"verify.mode":                {orgTighten, ""},
	"verify.answer_tool":         {orgTighten, ""},
	"verify.contract.protect":    {orgTighten, ""},
	"verify.contract.require":    {orgRefuse, "names commands the harness runs without asking"},
	"verify.contract.timeout_ms": {orgRefuse, "the machine's own setting"},
	"verify.critic":              {orgRefuse, "a critic model receives the conversation"},

	"additional_dirs":        {orgRefuse, "names folders on this machine"},
	"model.ollama.assist":    {orgTighten, ""},
	"completion.enabled":     {orgTighten, ""},
	"completion.local_only":  {orgTighten, ""},
	"model.allowed":          {orgTighten, ""},
	"mcp.allowed":            {orgTighten, ""},
	"mcp.denied":             {orgTighten, ""},
	"model.default":          {orgExtra, ""},
	"model.allowed_loopback": {orgExtra, ""},
	"model":                  {orgRefuse, "a provider and its base_url receive the code"},
	"custom_providers":       {orgRefuse, "a provider and its base_url receive the code"},
	"context":                {orgRefuse, "names files read into the prompt"},
	"rag":                    {orgRefuse, "a corpus URL and headers are egress"},
	"mcp":                    {orgRefuse, "an MCP server is a program or an endpoint"},
	"retrieval":              {orgRefuse, "embed_base_url receives the code"},
	"extensions":             {orgRefuse, "names programs to run on this machine"},
	"storage":                {orgRefuse, "configures storage and its credentials"},
	"server":                 {orgRefuse, "configures a server"},
	"control_plane":          {orgRefuse, "configures a server's control plane"},
	"schedules":              {orgRefuse, "configures a server's schedules"},
	"auth":                   {orgRefuse, "configures a server's sign-in"},
}

// orgRuleFor is the rule for path: its own, or its nearest section's.
func orgRuleFor(path string) (orgRule, bool) {
	for k := path; k != ""; {
		if r, ok := orgRules[k]; ok {
			return r, true
		}
		i := strings.LastIndex(k, ".")
		if i < 0 {
			break
		}
		k = k[:i]
	}
	return orgRule{kind: orgRefuse, why: "not classified for the org policy"}, false
}

// orgSetAside is a setting the org policy made that was left out.
type orgSetAside struct{ key, why string }

// orgStrip is what stripOrg needs to judge a setting: the configuration
// below the managed layer, the org document as a Config, and whether the
// machine's managed file pins the keys that signed it.
type orgStrip struct {
	below  *Config
	ws     *Config
	pinned bool
	// machineProviders are the providers the machine's managed file defines.
	machineProviders map[string]bool
}

// stripOrg removes from doc each setting the org policy may not make, or
// may make only to tighten what is below and here would loosen.
func stripOrg(doc map[string]any, j orgStrip) []orgSetAside {
	return stripOrgIn(doc, reflect.TypeFor[Config](), j)
}

// stripOrgIn is stripOrg over a document of root type root.
func stripOrgIn(doc map[string]any, root reflect.Type, j orgStrip) []orgSetAside {
	var out []orgSetAside
	var walk func(path string, obj map[string]any, t reflect.Type) bool
	walk = func(path string, obj map[string]any, t reflect.Type) bool {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		isStruct := t.Kind() == reflect.Struct && !reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]())
		if !isStruct {
			return false // the caller sets it aside whole
		}
		fields := jsonFields(t)
		for _, k := range slices.Sorted(maps.Keys(obj)) {
			f, ok := fieldFor(fields, k)
			if !ok || annotation(k) {
				continue
			}
			p := join(path, jsonName(f))
			if sub, ok := obj[k].(map[string]any); ok && hasOrgRuleUnder(p) {
				if !walk(p, sub, f.Type) {
					delete(obj, k)
					out = append(out, orgSetAside{key: p, why: "it cannot be judged setting by setting"})
				}
				continue
			}
			r, _ := orgRuleFor(p)
			if why := j.judge(p, r, obj[k]); why != "" {
				delete(obj, k)
				out = append(out, orgSetAside{key: p, why: why})
			}
		}
		return true
	}
	walk("", doc, root)
	return out
}

// hasOrgRuleUnder reports whether a rule names a setting inside section p.
func hasOrgRuleUnder(p string) bool {
	for k := range orgRules {
		if strings.HasPrefix(k, p+".") {
			return true
		}
	}
	return false
}

// judge says why the org policy may not set p to v, "" when it may.
func (j orgStrip) judge(p string, r orgRule, v any) string {
	pinNote := "the org policy may set it only when this machine's managed file pins org_policy.keys"
	switch r.kind {
	case orgRefuse:
		return "an org policy " + orRefuseWhy(r.why)
	case orgPinned:
		if j.pinned {
			return ""
		}
		return r.why + "; " + pinNote
	case orgExtra:
		return j.extra(p, v)
	}
	if p == "permissions.mode" || p == "mode.default" {
		if m, _ := v.(string); neverOrgMode(m) {
			return "an org policy never sets auto or bypass mode"
		}
	}
	wr, ok := workspaceRules[p]
	if !ok || wr.apply == nil || j.below == nil || j.ws == nil {
		return "no rule says how it tightens"
	}
	probe := *j.below
	if wr.apply(&probe, j.ws) || j.pinned {
		return ""
	}
	return "the org policy may only tighten it (" + wr.why + "); " + pinNote + " to loosen it"
}

func orRefuseWhy(why string) string {
	if strings.HasPrefix(why, "only ") || strings.HasPrefix(why, "the machine") {
		return "does not set it: " + why
	}
	return "does not set it: it " + why
}

// extra checks the settings an org policy may make outright.
func (j orgStrip) extra(p string, v any) string {
	switch p {
	case "record.ship.enabled", "record.ship.max_backlog_mb":
		return "" // sent only to the server this machine enrolled with
	case "hooks.managed_only", "studio.disable_host_terminal":
		if b, ok := v.(bool); ok && b {
			return ""
		}
		return "an org policy may only turn it on"
	case "model.allowed_loopback":
		if b, ok := v.(bool); ok && !b {
			return ""
		}
		return "an org policy may only set it to false, which narrows"
	case "model.default":
		name, _ := v.(string)
		if j.machineProviders[name] {
			return ""
		}
		return "an org policy may pin the model only to a provider the machine's managed file defines, never a person's own"
	}
	return "not classified for the org policy"
}

// unionLists are the managed settings both sources' entries are kept of: a
// deny or ask rule, a honeytoken or a protected file is only ever added.
var unionLists = map[string]bool{"permissions.deny": true, "permissions.ask": true,
	"monitor.honeytokens": true, "verify.contract.protect": true}

// orgJoinedBack are the union-ruled settings whose org entries are joined
// with the layers below, never replacing them: deny and ask in applyManaged,
// honeytokens in monitorBelow, the rest in joinManagedLists. Every union-ruled
// tighten key must be here; TestUnionRuledOrgKeysAreJoinedBack holds it.
var orgJoinedBack = map[string]bool{"permissions.deny": true, "permissions.ask": true,
	"monitor.honeytokens": true, "verify.contract.protect": true}

// orgBelowLists are the lists joinOrgLists joins back, by key.
var orgBelowLists = []string{"verify.contract.protect"}

// joinManagedLists joins back the lists below that the managed layer set,
// the machine's file or the org policy: a null, empty or other list from it
// never drops the person's own entries, so these lists only grow.
func joinManagedLists(cfg *Config, below map[string][]string) {
	for _, k := range orgBelowLists {
		if !cfg.ManagedSets(k) {
			continue
		}
		dst := unionFields[k](cfg)
		*dst = dedupe(append(slices.Clone(*dst), below[k]...))
	}
}

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
	ws := Default()
	if err := json.Unmarshal(doc, &ws); err != nil {
		return err
	}
	below := Default()
	// Judged as if pinned, so every setting it could carry is checked.
	stripOrg(m, orgStrip{below: &below, ws: &ws, pinned: true})
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
	ws := Default()
	if err := json.Unmarshal(e.Doc, &ws); e.Doc != nil && err != nil {
		doc, e.Fallback = map[string]any{}, true
		st.Status, st.Reason = orgpolicy.StatusFallback, "the org policy does not read: "+err.Error()
	}
	j := orgStrip{below: cfg, ws: &ws, pinned: len(mc.Keys) > 0, machineProviders: machineProviders(local)}
	for _, s := range stripOrg(doc, j) {
		cfg.SetAside = append(cfg.SetAside, SetAsideKey{File: source, Layer: LayerOrg, Key: s.key, Reason: s.why})
	}
	if e.Fallback {
		// Of a stale or refused policy only what tightens stays: its deny and
		// ask rules and its allow lists.
		doc = pickTightening(doc)
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

// warnOrgFallback says the fallback applies, and why, and that a required
// org policy whose keys the machine does not pin trusts its first answer.
func warnOrgFallback(w warnSink, c Config) {
	if c.Org.Required && len(c.Org.Keys) == 0 && c.ManagedSets("org_policy") {
		w.emit("orgpolicy\x00unpinned", OrgUnpinnedWarning)
	}
	if c.OrgPolicy.Status == orgpolicy.StatusFallback {
		w.emit("orgpolicy\x00fallback", "the org policy fallback applies (plan mode, no allow rules, no web tools): "+c.OrgPolicy.Reason)
	}
}

// OrgUnpinnedWarning is said of a required org policy with no pinned keys.
const OrgUnpinnedWarning = "org_policy.required is set without org_policy.keys: the signing key is pinned at the first sign-in, so whoever answers it decides the key; pin the org's keys in org_policy.keys"

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

// orgListKeys are the allow lists an org policy keeps as a layer of its own,
// so they narrow the machine's lists and never replace them.
var orgListKeys = [][2]string{{"model", "allowed"}, {"mcp", "allowed"}, {"mcp", "denied"}}

// takeOrgLists moves the allow lists out of the org document into their own
// document, nil when it sets none.
func takeOrgLists(doc map[string]any) map[string]any {
	var out map[string]any
	for _, kv := range orgListKeys {
		sec, ok := lookupFold(doc, kv[0]).(map[string]any)
		if !ok {
			continue
		}
		for k, v := range sec {
			if foldKey(k) != foldKey(kv[1]) {
				continue
			}
			if out == nil {
				out = map[string]any{}
			}
			dst, _ := out[kv[0]].(map[string]any)
			if dst == nil {
				dst = map[string]any{}
				out[kv[0]] = dst
			}
			dst[kv[1]] = v
			delete(sec, k)
		}
	}
	return out
}

// lookupFold is doc's value under key, matched as the config decode does.
func lookupFold(doc map[string]any, key string) any {
	for k, v := range doc {
		if foldKey(k) == foldKey(key) {
			return v
		}
	}
	return nil
}

// noteOrgLists adds the org policy's allow lists as a layer of their own:
// every layer must admit a model or server, so they only narrow.
func noteOrgLists(cfg *Config, lists map[string]any, source string) error {
	b, err := json.Marshal(lists)
	if err != nil {
		return err
	}
	var src Config
	if err := json.Unmarshal(b, &src); err != nil {
		return fmt.Errorf("%s: %w", source, err)
	}
	cfg.orgListFile = source
	noteAllowLayerFrom(cfg, &src, source, managedKeys(b))
	cfg.OrgPolicy.Keys = append(cfg.OrgPolicy.Keys, managedKeys(b)...)
	sort.Strings(cfg.OrgPolicy.Keys)
	return nil
}

// pickTightening keeps of doc only its deny and ask rules and allow lists.
func pickTightening(doc map[string]any) map[string]any {
	out := map[string]any{}
	for _, kv := range [][2]string{{"permissions", "deny"}, {"permissions", "ask"}, {"model", "allowed"}, {"mcp", "allowed"}, {"mcp", "denied"}} {
		sec, ok := lookupFold(doc, kv[0]).(map[string]any)
		if !ok {
			continue
		}
		for k, v := range sec {
			if foldKey(k) == foldKey(kv[1]) {
				dst, _ := out[kv[0]].(map[string]any)
				if dst == nil {
					dst = map[string]any{}
					out[kv[0]] = dst
				}
				dst[kv[1]] = v
			}
		}
	}
	return out
}

// neverOrgMode reports a mode an org policy never sets, however it is written.
func neverOrgMode(m string) bool {
	n := strings.ToLower(strings.TrimSpace(m))
	return n == "auto" || strings.HasPrefix(n, "bypass")
}

// machineProviders are the provider names the machine's managed file defines.
func machineProviders(local []byte) map[string]bool {
	var m struct {
		Model struct {
			Providers map[string]json.RawMessage `json:"providers"`
		} `json:"model"`
	}
	out := map[string]bool{}
	if local != nil && json.Unmarshal(local, &m) == nil {
		for k := range m.Model.Providers {
			out[k] = true
		}
	}
	return out
}
