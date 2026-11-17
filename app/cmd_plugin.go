package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/plugin"
	"github.com/zybuu-ai/abhed/profiles"
)

// pluginIO is what `abhed plugin` runs with.
type pluginIO struct {
	in       io.Reader
	out, err io.Writer
	// terminal is whether in is a person at a terminal: only one approves
	// without naming the digest.
	terminal bool
	// inAgent says why this runs in an agent's command, or "": such a
	// command never changes plugins.
	inAgent string
	// fetch gets a URL's body; nil fetches over HTTPS with the profile
	// signed in to that server, when there is one.
	fetch func(ctx context.Context, raw string, limit int64) ([]byte, error)
}

func stdPluginIO() pluginIO {
	return pluginIO{in: os.Stdin, out: os.Stdout, err: os.Stderr, terminal: term.IsTerminal(int(os.Stdin.Fd())), inAgent: inAgentCommand()}
}

const pluginUsage = `usage: abhed plugin list [-json]
       abhed plugin show NAME
       abhed plugin install [-sha256 DIGEST] PATH|URL|NAME[@VERSION]
       abhed plugin approve [-sha256 DIGEST] NAME
       abhed plugin remove [-yes] NAME
       abhed plugin publishers list | add -issuer URL IDENTITY | remove IDENTITY
       abhed plugin trusted-root FILE
       abhed plugin pack DIR
  A plugin bundles agents, skills, commands, hooks and MCP servers under one
  manifest. Installed content stays off until you approve that exact content.
`

// pluginCmd is `abhed plugin`.
func pluginCmd(workspace string, args []string, trust config.TrustChoice, pio pluginIO) int {
	fail := func(err error) int { fmt.Fprintf(pio.err, "abhed: %v\n", err); return 1 }
	usage := func() int { fmt.Fprint(pio.err, pluginUsage); return 2 }
	if len(args) == 0 {
		return usage()
	}
	changes := map[string]bool{"install": true, "approve": true, "remove": true, "rm": true, "uninstall": true, "publishers": true, "trusted-root": true}
	if changes[args[0]] && pio.inAgent != "" && !(args[0] == "publishers" && len(args) > 1 && args[1] == "list") {
		return fail(fmt.Errorf("plugin %s is refused inside an agent's command (%s); plugins change only from your own terminal", args[0], pio.inAgent))
	}
	store, err := plugin.DefaultStore()
	if err != nil {
		return fail(err)
	}
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust, Quiet: true, Warn: func(string) {}})
	if err != nil {
		return fail(err)
	}
	pc := &pluginCtx{io: pio, store: store, cfg: cfg}
	rest := args[1:]
	switch args[0] {
	case "list", "ls":
		fs := flag.NewFlagSet("plugin list", flag.ContinueOnError)
		fs.SetOutput(pio.err)
		asJSON := fs.Bool("json", false, "print JSON")
		if fs.Parse(rest) != nil || fs.NArg() != 0 {
			return usage()
		}
		return fail0(pc.list(*asJSON), fail)
	case "show":
		if len(rest) != 1 {
			return usage()
		}
		return fail0(pc.show(rest[0]), fail)
	case "install":
		fs := flag.NewFlagSet("plugin install", flag.ContinueOnError)
		fs.SetOutput(pio.err)
		pin := fs.String("sha256", "", "trust an unsigned bundle whose manifest has this SHA-256")
		if fs.Parse(rest) != nil || fs.NArg() != 1 {
			return usage()
		}
		return fail0(pc.install(fs.Arg(0), strings.ToLower(*pin)), fail)
	case "approve":
		fs := flag.NewFlagSet("plugin approve", flag.ContinueOnError)
		fs.SetOutput(pio.err)
		digest := fs.String("sha256", "", "approve without a prompt, only if the waiting content has this digest")
		if fs.Parse(rest) != nil || fs.NArg() != 1 {
			return usage()
		}
		return fail0(pc.approve(fs.Arg(0), strings.ToLower(*digest)), fail)
	case "remove", "rm", "uninstall":
		fs := flag.NewFlagSet("plugin remove", flag.ContinueOnError)
		fs.SetOutput(pio.err)
		yes := fs.Bool("yes", false, "remove without asking")
		if fs.Parse(rest) != nil || fs.NArg() != 1 {
			return usage()
		}
		return fail0(pc.remove(fs.Arg(0), *yes), fail)
	case "publishers":
		return pc.publishers(rest, usage, fail)
	case "trusted-root":
		if len(rest) != 1 {
			return usage()
		}
		return fail0(pc.setTrustedRoot(rest[0]), fail)
	case "pack":
		if len(rest) != 1 {
			return usage()
		}
		return fail0(packDir(rest[0], pio.out), fail)
	}
	return usage()
}

type pluginCtx struct {
	io    pluginIO
	store *plugin.Store
	cfg   config.Config
}

// trustedRootFile is the person's own copy of a trusted root.
func (pc *pluginCtx) trustedRootFile() string { return pc.store.TrustedRootFile() }

// trustedRoot is the root a signature is checked against, here and at
// every load: the managed one, or else the person's own.
func (pc *pluginCtx) trustedRoot() (*plugin.TrustedRoot, error) {
	return pc.cfg.PluginTrustedRoot(pc.store)
}

// logChange puts a plugin change in ~/.abhed/config-changes.jsonl, which the
// next session records as config.changed.
func (pc *pluginCtx) logChange(by, name, basis, digest, refused string) {
	if err := writeConfigChange(configChange{By: by, Key: "plugins", Name: name, Kind: basis, SHA256: digest,
		File: pc.store.StateFile, Refused: refused}); err != nil {
		fmt.Fprintf(pc.io.err, "abhed: warning: the change log: %v\n", err)
	}
}

// fetched is a bundle read from where install was pointed, with what the
// fetch itself vouches for.
type fetched struct {
	bundle *plugin.Bundle
	from   string
	source string
}

func (pc *pluginCtx) fetch(arg string) (*fetched, error) {
	pol := pc.cfg.PluginPolicy()
	if info, err := os.Lstat(arg); err == nil {
		if len(pol.Sources) > 0 {
			return nil, errors.New("the managed configuration names plugin sources (plugins.sources), so plugins install only from them")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a link; name the bundle itself", arg)
		}
		abs, _ := filepath.Abs(arg)
		b, err := plugin.Open(arg)
		if err != nil {
			return nil, err
		}
		return &fetched{bundle: b, from: abs}, nil
	}
	if strings.HasPrefix(arg, "https://") || strings.HasPrefix(arg, "http://") {
		src := pol.SourceFor(arg)
		if len(pol.Sources) > 0 && src == "" {
			return nil, fmt.Errorf("%s is not under a plugin source the managed configuration names (plugins.sources)", arg)
		}
		b, err := pc.download(arg)
		if err != nil {
			return nil, err
		}
		// A URL under a source is still only a URL: the index vouches for a
		// digest, not the place.
		return &fetched{bundle: b, from: arg}, nil
	}
	name, version, _ := strings.Cut(arg, "@")
	if !pluginNameRE.MatchString(name) {
		return nil, fmt.Errorf("%s is not a file, a URL or a plugin name", arg)
	}
	if len(pol.Sources) == 0 {
		return nil, fmt.Errorf("no file %s, and no plugin source is configured to look the name up in", arg)
	}
	var tried []string
	for _, s := range pol.Sources {
		entry, err := pc.lookup(s, name, version)
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s: %v", s.Name, err))
			continue
		}
		bundleURL, err := resolveURL(s.URL, entry.URL)
		if err != nil || pol.SourceFor(bundleURL) != s.Name {
			return nil, fmt.Errorf("the source %s lists %s at %s, which is outside it", s.Name, name, entry.URL)
		}
		b, err := pc.download(bundleURL)
		if err != nil {
			return nil, err
		}
		if b.Digest != entry.SHA256 || b.Manifest.Name != name {
			return nil, fmt.Errorf("the bundle at %s is not the one the source %s lists (digest %s, listed %s)", bundleURL, s.Name, b.Digest, entry.SHA256)
		}
		return &fetched{bundle: b, from: bundleURL, source: s.Name}, nil
	}
	return nil, fmt.Errorf("no plugin source has %s: %s", arg, strings.Join(tried, "; "))
}

var pluginNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}$`)

func (pc *pluginCtx) lookup(s plugin.Source, name, version string) (*plugin.IndexEntry, error) {
	body, err := pc.get(s.URL, 4<<20)
	if err != nil {
		return nil, err
	}
	var idx plugin.Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("the index: %w", err)
	}
	var found *plugin.IndexEntry
	for i, e := range idx.Plugins {
		if e.Name != name || (version != "" && e.Version != version) {
			continue
		}
		// A version that does not read is never taken for the newest.
		if _, err := plugin.ParseVersion(e.Version); err != nil {
			continue
		}
		if found == nil || plugin.AtLeast(e.Version, found.Version) {
			found = &idx.Plugins[i]
		}
	}
	if found == nil {
		return nil, errors.New("not listed")
	}
	return found, nil
}

func resolveURL(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}

func (pc *pluginCtx) download(raw string) (*plugin.Bundle, error) {
	body, err := pc.get(raw, plugin.MaxTotal)
	if err != nil {
		return nil, err
	}
	files, err := plugin.ReadArchive(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	return plugin.FromFiles(files)
}

func (pc *pluginCtx) get(raw string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if pc.io.fetch != nil {
		return pc.io.fetch(ctx, raw, limit)
	}
	return fetchWithProfile(ctx, raw, limit)
}

// fetchWithProfile gets raw over HTTPS (plain http only to this machine),
// carrying the token of the profile signed in to that server, if any.
func fetchWithProfile(ctx context.Context, raw string, limit int64) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && profiles.Loopback(u.Hostname()))) {
		return nil, fmt.Errorf("%s: plugins are fetched over https", raw)
	}
	var prof *profiles.Profile
	if path, err := profiles.DefaultPath(); err == nil {
		if reg, err := profiles.Load(path); err == nil {
			for _, n := range reg.Names() {
				p := reg.Profiles[n]
				if p != nil && (raw == p.URL || strings.HasPrefix(raw, p.URL+"/")) {
					prof = p
					break
				}
			}
		}
	}
	var resp *http.Response
	if prof != nil {
		hc, err := prof.HTTPClient()
		if err != nil {
			return nil, err
		}
		resp, err = prof.Do(ctx, hc, func() (*http.Request, error) {
			req, err := http.NewRequest(http.MethodGet, raw, nil)
			if err == nil && prof.Token != "" {
				req.Header.Set("Authorization", "Bearer "+prof.Token)
			}
			return req, err
		}, nil)
		if err != nil {
			return nil, err
		}
	} else {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return nil, err
		}
		hc := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if r.URL.Host != via[0].URL.Host || r.URL.Scheme != via[0].URL.Scheme {
				return errors.New("a redirect to another server is not followed")
			}
			return nil
		}}
		if resp, err = hc.Do(req); err != nil {
			return nil, err
		}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", raw, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s is over %d bytes", raw, limit)
	}
	return body, nil
}

func (pc *pluginCtx) install(arg, pin string) error {
	if pc.cfg.Plugins.Disabled {
		return plugin.ErrDisabled
	}
	if pin != "" && !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(pin) {
		return errors.New("-sha256 takes the manifest's SHA-256, 64 hex digits")
	}
	f, err := pc.fetch(arg)
	if err != nil {
		return err
	}
	b := f.bundle
	name := b.Manifest.Name
	ev := plugin.Evidence{Digest: b.Digest, Pin: pin, Source: f.source}
	if b.Signature != nil {
		root, err := pc.trustedRoot()
		if err != nil {
			return err
		}
		if root == nil {
			return errors.New("the bundle is signed, but no Sigstore trusted root is set to check it; keep one with abhed plugin trusted-root FILE " +
				"(it is checked again at every load, so it is kept, not given per install)")
		}
		s, err := plugin.VerifySignature(root, b.ManifestBytes, b.Signature, time.Now())
		if err != nil {
			pc.logChange("plugin install", name, "", b.Digest, "signature: "+err.Error())
			return fmt.Errorf("the signature does not verify, so nothing was installed: %w", err)
		}
		ev.Signer = &s
	}
	unlock, err := pc.store.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := pc.store.Read()
	if err != nil {
		return err
	}
	basis, err := pc.cfg.PluginPolicy().Admit(ev, st.Publishers)
	if err != nil {
		pc.logChange("plugin install", name, "", b.Digest, err.Error())
		return fmt.Errorf("%s was not installed: %w", name, err)
	}
	if !plugin.AtLeast(runningVersion(), b.Manifest.Abhed) {
		return fmt.Errorf("%s %s needs Abhed %s or later", name, b.Manifest.Version, b.Manifest.Abhed)
	}
	out := pc.io.out
	for _, l := range plugin.Describe(b) {
		fmt.Fprintln(out, l)
	}
	fmt.Fprintf(out, "Trusted by: %s\n", basisText(basis, ev.Signer))
	by := "plugin install"
	if e := st.Plugins[name]; e != nil && e.Approved != nil {
		old, err := pc.store.Open(name, e.Approved)
		if err == nil {
			fmt.Fprintln(out, "Changes from the approved version:")
			for _, l := range plugin.Diff(old, b) {
				fmt.Fprintln(out, l)
			}
		}
		by = "plugin upgrade"
	}
	inst, err := pc.store.Stage(st, b, plugin.Install{Basis: basis, Signer: ev.Signer, Pin: pin, Source: f.source, From: f.from})
	if err != nil {
		return err
	}
	if err := pc.store.Write(st); err != nil {
		return err
	}
	pc.logChange(by, name, basis, b.Digest, "")
	fmt.Fprintf(out, "Installed %s %s, waiting for approval: it does not load until approved.\n", name, b.Manifest.Version)
	if !pc.io.terminal {
		fmt.Fprintf(out, "Approve it with: abhed plugin approve %s   (or -sha256 %s %s)\n", name, inst.Digest, name)
		return nil
	}
	if !pc.ask(fmt.Sprintf("Approve %s %s now? [y/N] ", name, b.Manifest.Version)) {
		fmt.Fprintf(out, "Not approved. Approve later with: abhed plugin approve %s\n", name)
		return nil
	}
	return pc.approveLocked(st, name, inst.Digest)
}

func basisText(basis string, s *plugin.Signer) string {
	switch basis {
	case plugin.BasisSignature:
		return "a Sigstore signature from " + config.Printable(s.String())
	case plugin.BasisPin:
		return "your -sha256 pin"
	case plugin.BasisOrgPin:
		return "a digest the managed configuration or a pinned org policy lists (plugins.allowed_bundles)"
	case plugin.BasisSource:
		return "the organisation's plugin source"
	}
	return basis
}

func (pc *pluginCtx) ask(q string) bool {
	fmt.Fprint(pc.io.out, q)
	line, _ := readAnswer(pc.io.in)
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

func (pc *pluginCtx) approve(name, digest string) error {
	unlock, err := pc.store.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := pc.store.Read()
	if err != nil {
		return err
	}
	e := st.Plugins[name]
	if e == nil || e.Pending == nil {
		return fmt.Errorf("%s has nothing waiting for approval", name)
	}
	b, err := pc.store.Open(name, e.Pending)
	if err != nil {
		return err
	}
	root, err := pc.trustedRoot()
	if err != nil {
		return err
	}
	ev, note := plugin.Judge(b, e.Pending, root, time.Now())
	if _, err := pc.cfg.PluginPolicy().Admit(ev, st.Publishers); err != nil {
		if note != "" {
			err = fmt.Errorf("%w (%s)", err, note)
		}
		return fmt.Errorf("%s cannot be approved: %w", name, err)
	}
	for _, l := range plugin.Describe(b) {
		fmt.Fprintln(pc.io.out, l)
	}
	if digest == "" {
		if !pc.io.terminal {
			return fmt.Errorf("not at a terminal: approve this exact content with -sha256 %s", e.Pending.Digest)
		}
		if !pc.ask(fmt.Sprintf("Approve %s %s? [y/N] ", name, e.Pending.Version)) {
			fmt.Fprintln(pc.io.out, "Not approved.")
			return nil
		}
		digest = e.Pending.Digest
	}
	return pc.approveLocked(st, name, digest)
}

func (pc *pluginCtx) approveLocked(st *plugin.State, name, digest string) error {
	old, now, err := pc.store.Approve(st, name, digest)
	if err != nil {
		return err
	}
	if err := pc.store.Write(st); err != nil {
		return err
	}
	pc.logChange("plugin approve", name, now.Basis, now.Digest, "")
	if old != nil {
		fmt.Fprintf(pc.io.out, "Approved %s %s (was %s); it loads from the next session.\n", name, now.Version, old.Version)
	} else {
		fmt.Fprintf(pc.io.out, "Approved %s %s; it loads from the next session.\n", name, now.Version)
	}
	return nil
}

func (pc *pluginCtx) remove(name string, yes bool) error {
	unlock, err := pc.store.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := pc.store.Read()
	if err != nil {
		return err
	}
	if st.Plugins[name] == nil {
		return fmt.Errorf("%s is not installed", name)
	}
	if !yes {
		if !pc.io.terminal {
			return errors.New("not at a terminal: add -yes to remove it")
		}
		if !pc.ask(fmt.Sprintf("Remove %s and all its files? [y/N] ", name)) {
			fmt.Fprintln(pc.io.out, "Nothing was removed.")
			return nil
		}
	}
	e, err := pc.store.Remove(st, name)
	if err != nil {
		return err
	}
	if err := pc.store.Write(st); err != nil {
		return err
	}
	digest := ""
	if e.Approved != nil {
		digest = e.Approved.Digest
	} else if e.Pending != nil {
		digest = e.Pending.Digest
	}
	pc.logChange("plugin remove", name, "", digest, "")
	fmt.Fprintf(pc.io.out, "Removed %s.\n", name)
	return nil
}

// pluginRow is one plugin as list shows it.
type pluginRow struct {
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	State    string `json:"state"`
	Basis    string `json:"basis,omitempty"`
	Signer   string `json:"signer,omitempty"`
	Digest   string `json:"digest,omitempty"`
	Pending  string `json:"pending_version,omitempty"`
	Why      string `json:"why,omitempty"`
	Location string `json:"path,omitempty"`
}

func (pc *pluginCtx) list(asJSON bool) error {
	st, err := pc.store.Read()
	if err != nil {
		return err
	}
	loaded := map[string]bool{}
	for _, l := range pc.cfg.LoadedPlugins() {
		loaded[l.Name] = true
	}
	var rows []pluginRow
	for _, n := range st.Names() {
		e := st.Plugins[n]
		r := pluginRow{Name: n}
		cur := e.Approved
		switch {
		case e.Approved == nil:
			cur, r.State = e.Pending, "pending"
		case loaded[n]:
			r.State = "loaded"
		default:
			r.State = "not loaded"
			if b, err := pc.store.Open(n, e.Approved); err != nil {
				r.Why = err.Error()
			} else {
				root, _ := pc.trustedRoot()
				ev, note := plugin.Judge(b, e.Approved, root, time.Now())
				if _, err := pc.cfg.PluginPolicy().Admit(ev, st.Publishers); err != nil {
					r.Why = err.Error()
					if note != "" {
						r.Why += " (" + note + ")"
					}
				}
			}
		}
		r.Version, r.Basis, r.Digest, r.Location = cur.Version, cur.Basis, cur.Digest, pc.store.Path(n, cur)
		// What the decisions file says; a load trusts only what it verifies again.
		if cur.Signer != nil {
			r.Signer = cur.Signer.String()
		}
		if e.Approved != nil && e.Pending != nil {
			r.Pending = e.Pending.Version
		}
		rows = append(rows, r)
	}
	if asJSON {
		enc := json.NewEncoder(pc.io.out)
		enc.SetIndent("", "  ")
		if rows == nil {
			rows = []pluginRow{}
		}
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(pc.io.out, "No plugins installed.")
		return nil
	}
	tw := tabwriter.NewWriter(pc.io.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tSTATE\tTRUSTED BY")
	for _, r := range rows {
		state := r.State
		if r.Pending != "" {
			state += " (" + r.Pending + " waiting)"
		}
		by := r.Basis
		if r.Signer != "" {
			by += ": " + r.Signer
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Version, state, config.Printable(by))
	}
	_ = tw.Flush()
	for _, r := range rows {
		if r.Why != "" {
			fmt.Fprintf(pc.io.out, "%s: %s\n", r.Name, config.Printable(r.Why))
		}
	}
	return nil
}

func (pc *pluginCtx) show(name string) error {
	st, err := pc.store.Read()
	if err != nil {
		return err
	}
	e := st.Plugins[name]
	if e == nil {
		return fmt.Errorf("%s is not installed", name)
	}
	for _, x := range []struct {
		label string
		inst  *plugin.Install
	}{{"Approved", e.Approved}, {"Waiting for approval", e.Pending}} {
		if x.inst == nil {
			continue
		}
		b, err := pc.store.Open(name, x.inst)
		if err != nil {
			fmt.Fprintf(pc.io.out, "%s: %v\n", x.label, err)
			continue
		}
		fmt.Fprintf(pc.io.out, "%s (%s):\n", x.label, pc.store.Path(name, x.inst))
		for _, l := range plugin.Describe(b) {
			fmt.Fprintln(pc.io.out, "  "+l)
		}
	}
	return nil
}

func (pc *pluginCtx) publishers(args []string, usage func() int, fail func(error) int) int {
	if len(args) == 0 {
		return usage()
	}
	change := func(f func(st *plugin.State) error) int {
		unlock, err := pc.store.Lock()
		if err != nil {
			return fail(err)
		}
		defer unlock()
		st, err := pc.store.Read()
		if err != nil {
			return fail(err)
		}
		if err := f(st); err != nil {
			return fail(err)
		}
		return fail0(pc.store.Write(st), fail)
	}
	switch args[0] {
	case "list":
		st, err := pc.store.Read()
		if err != nil {
			return fail(err)
		}
		if len(pc.cfg.Plugins.AllowedPublishers) > 0 || len(pc.cfg.Plugins.AllowedBundles) > 0 {
			fmt.Fprintln(pc.io.out, "The managed configuration decides (plugins.allowed_publishers):")
			for _, p := range pc.cfg.Plugins.AllowedPublishers {
				fmt.Fprintln(pc.io.out, "  "+p.String())
			}
			if len(st.Publishers) > 0 {
				fmt.Fprintln(pc.io.out, "Yours, not consulted while it does:")
			}
		}
		for _, p := range st.Publishers {
			fmt.Fprintln(pc.io.out, "  "+p.String())
		}
		return 0
	case "add":
		fs := flag.NewFlagSet("plugin publishers add", flag.ContinueOnError)
		fs.SetOutput(pc.io.err)
		iss := fs.String("issuer", "", "the OIDC issuer URL the signer's certificate names")
		if fs.Parse(args[1:]) != nil || fs.NArg() != 1 || *iss == "" {
			return usage()
		}
		p := plugin.Publisher{Identity: fs.Arg(0), Issuer: *iss}
		if err := p.Valid(); err != nil {
			return fail(err)
		}
		return change(func(st *plugin.State) error {
			for _, x := range st.Publishers {
				if x == p {
					return nil
				}
			}
			st.Publishers = append(st.Publishers, p)
			pc.logChange("plugin publishers add", p.Identity, "", "", "")
			fmt.Fprintf(pc.io.out, "You now trust bundles signed by %s.\n", p)
			return nil
		})
	case "remove", "rm":
		if len(args) != 2 {
			return usage()
		}
		return change(func(st *plugin.State) error {
			kept := st.Publishers[:0]
			for _, x := range st.Publishers {
				if x.Identity != args[1] {
					kept = append(kept, x)
				}
			}
			if len(kept) == len(st.Publishers) {
				return fmt.Errorf("%s is not one of your publishers", args[1])
			}
			st.Publishers = kept
			pc.logChange("plugin publishers remove", args[1], "", "", "")
			return nil
		})
	}
	return usage()
}

// setTrustedRoot copies a trusted root into ~/.abhed, so a later change to
// the file named (in a workspace, say) changes nothing.
func (pc *pluginCtx) setTrustedRoot(p string) error {
	data, err := os.ReadFile(p) // #nosec G304 -- the file the person named
	if err != nil {
		return err
	}
	if _, err := plugin.ParseTrustedRoot(data); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pc.trustedRootFile()), 0o700); err != nil {
		return err
	}
	tmp := pc.trustedRootFile() + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- beside the person's own state
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, pc.trustedRootFile()); err != nil {
		return err
	}
	pc.logChange("plugin trusted-root", "", "", plugin.Digest(data), "")
	fmt.Fprintf(pc.io.out, "Signatures are now checked against %s.\n", pc.trustedRootFile())
	if pc.cfg.Plugins.TrustedRoot != "" {
		fmt.Fprintln(pc.io.out, "The managed configuration names its own trusted root, which is used instead while it does.")
	}
	return nil
}

// packDir rewrites DIR's manifest with every file's hash and the
// capabilities its content uses, for a publisher to sign.
func packDir(dir string, out io.Writer) error {
	mp := filepath.Join(dir, plugin.ManifestName)
	data, err := os.ReadFile(mp) // #nosec G304 -- the publisher's own bundle
	if err != nil {
		return fmt.Errorf("pack reads the manifest's name, version and publisher from %s: %w", plugin.ManifestName, err)
	}
	var m plugin.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("%s: %w", mp, err)
	}
	files := map[string][]byte{}
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// .git and the like are left out, as Open leaves them out.
			if rel, _ := filepath.Rel(dir, p); rel != "." && !strings.Contains(filepath.ToSlash(rel), "/") && strings.HasPrefix(rel, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is a link or special file; a bundle holds regular files only", p)
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == plugin.ManifestName || rel == plugin.SignatureName {
			return nil
		}
		b, err := os.ReadFile(p) // #nosec G304 G122 -- the publisher's own bundle, packed before signing
		files[rel] = b
		return err
	})
	if err != nil {
		return err
	}
	packed, err := plugin.Pack(m, files)
	if err != nil {
		return err
	}
	if err := os.WriteFile(mp, packed, 0o644); err != nil { // #nosec G306 -- a file to publish
		return err
	}
	fmt.Fprintf(out, "Wrote %s (sha256 %s). Sign it with:\n  cosign sign-blob --bundle %s %s\n", mp, plugin.Digest(packed),
		filepath.Join(dir, plugin.SignatureName), mp)
	return nil
}

// runningVersion is this binary's version, "" when unset.
func runningVersion() string {
	v, _ := productVersion.Load().(string)
	return v
}
