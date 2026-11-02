package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/filelock"
	"github.com/zybuu-ai/abhed/orgpolicy"
	"github.com/zybuu-ai/abhed/profiles"
)

// Paths an org policy server answers.
const (
	orgPolicyPath     = "/v1/org/policy"
	orgPolicyKeysPath = "/v1/org/policy/keys"
)

// How a session start refreshes the org policy: at most once a minute, and
// never waiting more than a few seconds for the server.
var (
	orgRefreshEvery = time.Minute
	orgFetchTimeout = 5 * time.Second
)

// errNoOrgPolicy is a server that serves no org policy.
var errNoOrgPolicy = errors.New("the server serves no org policy")

// orgGet fetches path from p's server with its token, refreshing the token
// as Do does and saving the profile when it changed.
func orgGet(ctx context.Context, profilesFile, name string, p *profiles.Profile, path string) (int, []byte, error) {
	client, err := p.HTTPClient()
	if err != nil {
		return 0, nil, err
	}
	resp, err := p.Do(ctx, client, func() (*http.Request, error) {
		return p.NewRequest(http.MethodGet, path, nil)
	}, func(q *profiles.Profile) error { return saveRefreshed(profilesFile, name, q) })
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, err
}

// saveRefreshed keeps a refreshed token in the profiles file, read again so
// another process's change to another profile is not lost.
func saveRefreshed(path, name string, p *profiles.Profile) error {
	reg, err := profiles.Load(path)
	if err != nil {
		return err
	}
	reg.Profiles[name] = p
	return profiles.Save(path, reg)
}

// orgKeys asks the server for its policy signing keys.
func orgKeys(ctx context.Context, profilesFile, name string, p *profiles.Profile) ([]string, error) {
	status, body, err := orgGet(ctx, profilesFile, name, p, orgPolicyKeysPath)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusNotImplemented:
		return nil, errNoOrgPolicy
	default:
		return nil, fmt.Errorf("the server answered %d for its policy keys", status)
	}
	var out struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Keys) == 0 {
		return nil, errors.New("the server's policy keys do not read")
	}
	if _, err := orgpolicy.ParseKeys(out.Keys); err != nil {
		return nil, fmt.Errorf("the server's policy keys: %w", err)
	}
	return out.Keys, nil
}

// orgFetch asks the server for the caller's signed policy.
func orgFetch(ctx context.Context, profilesFile, name string, p *profiles.Profile) (orgpolicy.Envelope, error) {
	var env orgpolicy.Envelope
	status, body, err := orgGet(ctx, profilesFile, name, p, orgPolicyPath)
	if err != nil {
		return env, fmt.Errorf("the org server cannot be reached: %w", err)
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusNotImplemented:
		return env, errNoOrgPolicy
	case http.StatusUnauthorized, http.StatusForbidden:
		return env, fmt.Errorf("the org server refused the sign-in (%d); run abhed login %s", status, p.URL)
	default:
		return env, fmt.Errorf("the org server answered %d", status)
	}
	var out struct {
		Envelope *orgpolicy.Envelope `json:"envelope"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Envelope == nil {
		return env, errors.New("the org server's answer holds no signed policy")
	}
	return *out.Envelope, nil
}

// lockOrgState holds the org policy state against other Abhed processes.
func lockOrgState(path string) (func(), error) {
	return filelock.Lock(path+".lock", 5*time.Second, "the org policy state %s is held by another Abhed (waited %s)")
}

// enrollOrgPolicy runs after a sign-in: when the server serves an org policy,
// it pins the signing keys, then fetches and verifies the policy.
// enrolOptions say how a sign-in may take an org policy: keys given out of
// band, and consent (the -org-policy flag, or a y/N answer).
type enrolOptions struct {
	given   []string
	accept  bool
	confirm func(prompt string) bool
}

func enrollOrgPolicy(ctx context.Context, profilesFile, name string, p *profiles.Profile, o enrolOptions, out, errOut io.Writer) error {
	given := o.given
	mc, err := config.MachineOrgPolicy()
	if err != nil {
		return err
	}
	machine := mc.Machine()
	if machine.URL != "" && machine.URL != p.URL {
		if len(given) > 0 {
			return fmt.Errorf("the managed configuration takes the org policy only from %s", machine.URL)
		}
		return nil
	}
	path, err := orgpolicy.Path()
	if err != nil {
		return err
	}
	unlock, err := lockOrgState(path)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := orgpolicy.LoadState(path)
	if err != nil {
		return err
	}
	if st != nil && st.URL != p.URL {
		fmt.Fprintf(errOut, "abhed: the org policy stays with %s; run abhed policy leave first to take %s's\n", st.URL, p.URL)
		return nil
	}
	served, err := orgKeys(ctx, profilesFile, name, p)
	if errors.Is(err, errNoOrgPolicy) {
		if len(given) > 0 {
			return fmt.Errorf("%s serves no org policy", p.URL)
		}
		return nil
	}
	if err != nil {
		return err
	}
	pins, err := choosePins(served, given, machine.Keys, st)
	if err != nil {
		return err
	}
	tofu := len(machine.Keys) == 0 && len(given) == 0 && (st == nil || len(st.Keys) == 0)
	if tofu && plainRemote(p.URL) {
		return fmt.Errorf("%s is plain http, so the policy key it offers could be anyone's; pass -policy-key with the org's key", p.URL)
	}
	// Taking a policy is the person's choice, unless the machine's managed file names this server.
	if machine.URL != p.URL && st == nil && !o.accept {
		prompt := fmt.Sprintf("%s serves an org policy signed by %s.\nTaking it lets that server govern this machine's Abhed settings as managed configuration,\nand it may turn on sending your session records to it. Take it? [y/N] ",
			p.URL, strings.Join(keyIDs(pins), ", "))
		if o.confirm == nil || !o.confirm(prompt) {
			fmt.Fprintf(out, "%s serves an org policy; not taken. abhed login -org-policy %s takes it.\n", p.URL, p.URL)
			return nil
		}
	}
	if st == nil {
		st = &orgpolicy.State{}
	}
	st.Profile, st.URL, st.Keys, st.Subject = name, p.URL, pins, p.Username
	env, err := orgFetch(ctx, profilesFile, name, p)
	st.LastAttempt = time.Now()
	if err != nil {
		st.LastError = err.Error()
		if serr := orgpolicy.SaveState(path, st); serr != nil {
			return serr
		}
		return err
	}
	stmt, aerr := orgpolicy.Accept(st, machine, env, p.Username, config.CheckOrgPolicy, time.Now())
	st.LastError = ""
	if aerr == nil && st.Tenant == "" {
		// The tenant is learnt with the key, from the first statement that verifies.
		st.Tenant = stmt.Tenant
	}
	if err := orgpolicy.SaveState(path, st); err != nil {
		return err
	}
	if aerr != nil {
		return fmt.Errorf("the org policy was refused, so the fallback applies: %w", aerr)
	}
	if len(machine.Keys) == 0 && len(given) == 0 {
		fmt.Fprintf(out, "Pinned %s's policy signing key%s %s; check %s with your administrator.\n",
			p.URL, plural(len(pins)), strings.Join(keyIDs(pins), ", "), map[bool]string{true: "them", false: "it"}[len(pins) > 1])
	}
	fmt.Fprintf(out, "Applying the org policy %q (version %d) from %s; abhed policy status shows it.\n", stmt.Name, stmt.Version, p.URL)
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// plainRemote reports whether u is plain http to another machine.
func plainRemote(u string) bool {
	pu, err := url.Parse(u)
	return err == nil && pu.Scheme == "http" && !profiles.Loopback(pu.Hostname())
}

// choosePins picks the keys to pin: the machine's, else those given (offered by
// the server), else those already pinned, else the server's as offered.
func choosePins(served, given, machine []string, st *orgpolicy.State) ([]string, error) {
	if len(machine) > 0 {
		return slices.Clone(machine), nil
	}
	offered := keyIDs(served)
	if len(given) > 0 {
		for _, g := range keyIDs(given) {
			if g == "" || !slices.Contains(offered, g) {
				return nil, errors.New("the server does not offer the policy key given with -policy-key")
			}
		}
		return slices.Clone(given), nil
	}
	if st != nil && len(st.Keys) > 0 {
		return st.Keys, nil
	}
	return served, nil
}

// keyIDs are the ids of base64 or PEM keys; one that does not read is "".
func keyIDs(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		if pub, err := orgpolicy.ParseKey(k); err == nil {
			out[i] = orgpolicy.KeyID(pub)
		}
	}
	return out
}

// orgRefreshResult is what a refresh did.
type orgRefreshResult struct {
	Enrolled bool
	Fetched  bool
	Changed  bool
	Stmt     orgpolicy.Statement
}

// refreshOrgPolicy fetches the enrolled org's policy again, unless force at
// most once per orgRefreshEvery; an unreachable server leaves the cache as is.
func refreshOrgPolicy(ctx context.Context, force bool) (orgRefreshResult, error) {
	var res orgRefreshResult
	path, err := orgpolicy.Path()
	if err != nil {
		return res, err
	}
	if st, err := orgpolicy.LoadState(path); err != nil || st == nil {
		return res, err
	} else if !force && time.Since(st.LastAttempt) < orgRefreshEvery {
		res.Enrolled = true
		return res, nil
	}
	unlock, err := lockOrgState(path)
	if err != nil {
		return res, err
	}
	defer unlock()
	st, err := orgpolicy.LoadState(path)
	if err != nil || st == nil {
		return res, err
	}
	res.Enrolled = true
	mc, err := config.MachineOrgPolicy()
	if err != nil {
		return res, err
	}
	if m := mc.Machine(); m.URL != "" && m.URL != st.URL {
		return res, fmt.Errorf("the managed configuration takes the org policy only from %s, not %s", m.URL, st.URL)
	}
	before := ""
	if st.Envelope != nil {
		before = st.Envelope.Payload
	}
	st.LastAttempt = time.Now()
	profilesFile, err := profiles.DefaultPath()
	if err != nil {
		return res, err
	}
	reg, err := profiles.Load(profilesFile)
	if err != nil {
		return res, err
	}
	p, ok := reg.Profiles[st.Profile]
	if !ok || p.URL != st.URL || p.Token == "" {
		st.LastError = fmt.Sprintf("the profile %s is not signed in to %s; run abhed login %s", st.Profile, st.URL, st.URL)
		return res, errors.Join(errors.New(st.LastError), orgpolicy.SaveState(path, st))
	}
	cctx, cancel := context.WithTimeout(ctx, orgFetchTimeout)
	env, ferr := orgFetch(cctx, profilesFile, st.Profile, p)
	cancel()
	if ferr != nil {
		st.LastError = ferr.Error()
		return res, errors.Join(ferr, orgpolicy.SaveState(path, st))
	}
	res.Fetched = true
	stmt, aerr := orgpolicy.Accept(st, mc.Machine(), env, p.Username, config.CheckOrgPolicy, time.Now())
	st.LastError = ""
	res.Stmt = stmt
	res.Changed = aerr == nil && st.Envelope != nil && st.Envelope.Payload != before && !samePolicy(before, stmt)
	if err := orgpolicy.SaveState(path, st); err != nil {
		return res, err
	}
	if aerr != nil {
		return res, fmt.Errorf("the fetched org policy was refused, so the fallback applies: %w", aerr)
	}
	return res, nil
}

// samePolicy reports whether the earlier payload held the same policy
// version and document as stmt, which a fresh signature alone does not change.
func samePolicy(beforePayload string, stmt orgpolicy.Statement) bool {
	if beforePayload == "" {
		return false
	}
	env := orgpolicy.Envelope{Payload: beforePayload}
	b, err := decodePayload(env)
	if err != nil {
		return false
	}
	var old orgpolicy.Statement
	if json.Unmarshal(b, &old) != nil {
		return false
	}
	return old.Name == stmt.Name && old.Version == stmt.Version && old.Digest() == stmt.Digest()
}

func decodePayload(e orgpolicy.Envelope) ([]byte, error) {
	return base64.StdEncoding.DecodeString(e.Payload)
}

// orgPolicyAtStart refreshes the org policy as a session starts, saying so
// only when it changed or could not be had.
func orgPolicyAtStart(ctx context.Context, errOut io.Writer) {
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := refreshOrgPolicy(ctx, false)
	switch {
	case err != nil && res.Enrolled:
		fmt.Fprintf(errOut, "abhed: org policy: %v; the cached policy applies until it is too old\n", err)
	case res.Changed:
		fmt.Fprintf(errOut, "abhed: org policy %q version %d applies\n", res.Stmt.Name, res.Stmt.Version)
	}
}

// keepOrgPolicyFresh refreshes the org policy every interval until ctx
// ends; a change applies to the sessions that start after it.
func keepOrgPolicyFresh(ctx context.Context, every time.Duration, errOut io.Writer) {
	if every <= 0 {
		return
	}
	var once sync.Once
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			res, err := refreshOrgPolicy(ctx, true)
			if !res.Enrolled {
				continue
			}
			if err != nil {
				once.Do(func() { fmt.Fprintf(errOut, "abhed: org policy: %v\n", err) })
				continue
			}
			if res.Changed {
				fmt.Fprintf(errOut, "abhed: the org policy changed (%q version %d); it applies to sessions started from now on\n", res.Stmt.Name, res.Stmt.Version)
			}
		}
	}()
}

// policyCmd is `abhed policy`: the signed org policy this machine takes.
func policyCmd(args []string, out, errOut io.Writer) int {
	usage := func() int {
		fmt.Fprintln(errOut, "usage: abhed policy status | show | refresh | leave")
		return 2
	}
	if len(args) != 1 {
		return usage()
	}
	fail := func(err error) int { fmt.Fprintf(errOut, "abhed: %v\n", err); return 1 }
	switch args[0] {
	case "status":
		return policyStatus(out, errOut)
	case "show":
		path, err := orgpolicy.Path()
		if err != nil {
			return fail(err)
		}
		st, err := orgpolicy.LoadState(path)
		if err != nil {
			return fail(err)
		}
		mc, err := config.MachineOrgPolicy()
		if err != nil {
			return fail(err)
		}
		e := orgpolicy.Evaluate(st, mc.Machine(), time.Now())
		if e.Statement == nil {
			return fail(errors.New("no verified org policy is held: " + e.Reason))
		}
		var v any
		_ = json.Unmarshal(e.Statement.Policy, &v)
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0
	case "refresh":
		if !profileNetwork(errOut) {
			return 1
		}
		res, err := refreshOrgPolicy(context.Background(), true)
		if !res.Enrolled && err == nil {
			fmt.Fprintln(out, "This machine takes no org policy; abhed login URL takes one from a server that serves it.")
			return 0
		}
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(out, "Org policy %q version %d (epoch %d) verified.\n", res.Stmt.Name, res.Stmt.Version, res.Stmt.Epoch)
		return 0
	case "leave":
		mc, err := config.MachineOrgPolicy()
		if err != nil {
			return fail(err)
		}
		if mc.Required || mc.URL != "" {
			return fail(errors.New("the managed configuration names the org policy for this machine; only its administrator can remove it"))
		}
		path, err := orgpolicy.Path()
		if err != nil {
			return fail(err)
		}
		unlock, err := lockOrgState(path)
		if err != nil {
			return fail(err)
		}
		defer unlock()
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		fmt.Fprintln(out, "This machine no longer takes an org policy.")
		return 0
	}
	return usage()
}

// policyStatus prints what applies, as loading decides it.
func policyStatus(out, errOut io.Writer) int {
	cfg, err := config.LoadManagedWith(config.LoadOptions{Warn: func(string) {}})
	if err != nil {
		fmt.Fprintf(errOut, "abhed: %v\n", err)
		return 1
	}
	s := cfg.OrgPolicy
	fmt.Fprintf(out, "org policy: %s\n", s.OrgPolicyLine())
	if s.URL != "" {
		fmt.Fprintf(out, "  server:   %s (profile %s)\n", s.URL, orDefault(s.Profile, "none"))
	}
	if path, err := orgpolicy.Path(); err == nil {
		if st, err := orgpolicy.LoadState(path); err == nil && st != nil {
			mc, _ := config.MachineOrgPolicy()
			pins := st.Keys
			from := "pinned at sign-in"
			if len(mc.Keys) > 0 {
				pins, from = mc.Keys, "set by the managed configuration"
			}
			fmt.Fprintf(out, "  keys:     %s (%s)\n", strings.Join(keyIDs(pins), ", "), from)
			if !st.FetchedAt.IsZero() {
				fmt.Fprintf(out, "  fetched:  %s\n", st.FetchedAt.Local().Format("2 Jan 2006 15:04"))
			}
			if st.LastError != "" {
				fmt.Fprintf(out, "  last try: %s: %s\n", st.LastAttempt.Local().Format("2 Jan 15:04"), st.LastError)
			}
		}
	}
	if len(s.Keys) > 0 {
		fmt.Fprintf(out, "  sets:     %s\n", strings.Join(s.Keys, ", "))
	}
	if len(s.Shadowed) > 0 {
		fmt.Fprintf(out, "  shadowed: %s (this machine's managed file sets them too, and wins)\n", strings.Join(s.Shadowed, ", "))
	}
	for _, k := range cfg.SetAside {
		if k.Layer == config.LayerOrg {
			fmt.Fprintf(out, "  refused:  %s: %s\n", k.Key, k.Reason)
		}
	}
	return 0
}

// policyKeyFlag collects -policy-key values.
type policyKeyFlag []string

func (p *policyKeyFlag) String() string     { return strings.Join(*p, ",") }
func (p *policyKeyFlag) Set(v string) error { *p = append(*p, v); return nil }

var _ flag.Value = (*policyKeyFlag)(nil)
