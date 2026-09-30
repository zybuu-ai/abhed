package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"golang.org/x/term"
)

// lazySandbox is a sandbox chosen off the start-up path. Probing a
// container runtime runs `podman info` or `docker info`, which took 2.6 s
// of blank screen with a podman machine that was not running. When the
// process tier is available and the configured minimum is no higher, the
// session can start at once: the answer can only be that tier or a
// stronger one. Commands wait for the answer; nothing runs unsandboxed.
type lazySandbox struct {
	done chan struct{}
	sb   sandbox.Sandbox
	err  error
	// floor is the tier the answer is known to be at least, "" when the
	// session waited for the answer.
	floor sandbox.Tier
}

// startSandbox begins choosing the sandbox. It returns at once when the
// answer cannot fail, and otherwise waits for it, so a configuration no
// backend meets still refuses to start.
func startSandbox(cfg config.Config, workspace string) (*lazySandbox, error) {
	p, err := sandboxconfig.Policy(cfg, workspace)
	if err != nil {
		return nil, err
	}
	l := &lazySandbox{done: make(chan struct{})}
	go func() {
		l.sb, l.err = sandbox.Select(p)
		close(l.done)
	}()
	if p.MinTier.Strength() <= sandbox.TierProcess.Strength() {
		if ok, _ := sandbox.NewProcess(p).Available(); ok {
			l.floor = sandbox.TierProcess
			return l, nil
		}
	}
	<-l.done
	return l, l.err
}

func (l *lazySandbox) wait() (sandbox.Sandbox, error) {
	<-l.done
	return l.sb, l.err
}

// Resolved reports whether the choice has been made.
func (l *lazySandbox) Resolved() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

func (l *lazySandbox) Tier() sandbox.Tier {
	if sb, err := l.wait(); err == nil {
		return sb.Tier()
	}
	return sandbox.TierNone
}

func (l *lazySandbox) Available() (bool, string) {
	sb, err := l.wait()
	if err != nil {
		return false, err.Error()
	}
	return sb.Available()
}

// Command waits for the choice; a failed choice gives a command that does
// not start, with the reason.
func (l *lazySandbox) Command(ctx context.Context, cwd, command string) *exec.Cmd {
	sb, err := l.wait()
	if err != nil {
		return &exec.Cmd{Err: fmt.Errorf("no sandbox: %w", err)}
	}
	return sb.Command(ctx, cwd, command)
}

func (l *lazySandbox) Describe() string {
	if sb, err := l.wait(); err == nil {
		return sb.Describe()
	}
	return "no sandbox"
}

// Label is the tier for the banner without waiting: the answer when it is
// in, else the tier it is known to be at least.
func (l *lazySandbox) Label() string {
	if l.Resolved() {
		return string(l.Tier())
	}
	return string(l.floor) + " or stronger (checking)"
}

// endpointProbe checks, off the start-up path, whether anything answers at
// the model's address, so a server that is down is named at once and a
// task fails in under a second rather than after the retries.
type endpointProbe struct {
	addr string // host:port; "" when the provider has no address to dial
	mu   sync.Mutex
	down error
	at   time.Time
	// first is closed when the first dial, started by start, has ended.
	first chan struct{}
	once  sync.Once
}

// start dials once in the background.
func (e *endpointProbe) start(ctx context.Context) {
	e.once.Do(func() {
		go func() {
			_ = e.run(ctx)
			close(e.first)
		}()
	})
}

// firstResult waits up to limit for the first dial and returns what it
// found: nil when the endpoint answered or the dial is still going.
func (e *endpointProbe) firstResult(limit time.Duration) error {
	select {
	case <-e.first:
	case <-time.After(limit):
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.down
}

// newEndpointProbe dials the provider's address in the background.
func newEndpointProbe(p config.ProviderConfig) *endpointProbe {
	return &endpointProbe{addr: dialAddr(p.BaseURL), first: make(chan struct{})}
}

// dialAddr is the host:port of a base URL, "" when it has none.
func dialAddr(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	if port == "" {
		return ""
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// run dials once, with a 500 ms limit, and remembers a refusal.
func (e *endpointProbe) run(ctx context.Context) error {
	if e.addr == "" {
		return nil
	}
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", e.addr)
	if conn != nil {
		_ = conn.Close()
	}
	var down error
	if err != nil && unreachable(err) {
		down = err
	}
	e.mu.Lock()
	e.down, e.at = down, time.Now()
	e.mu.Unlock()
	return down
}

// unreachable is a dial failure that retrying will not fix in seconds: a
// refused connection or a name that does not resolve. A timeout is left to
// the client, since a slow network is not a missing server.
func unreachable(err error) bool {
	var dns *net.DNSError
	return errors.Is(err, syscall.ECONNREFUSED) || (errors.As(err, &dns) && dns.IsNotFound)
}

// check is asked before each model call. When the last probe found the
// address down it dials again; still down, the call fails at once.
func (e *endpointProbe) check(ctx context.Context) error {
	e.mu.Lock()
	down := e.down
	e.mu.Unlock()
	if down == nil {
		return nil
	}
	return e.run(ctx)
}

// gatedAdapter fails a model call at once while its endpoint is known to
// be down, instead of spending the retries on it.
type gatedAdapter struct {
	model.Adapter
	probe *endpointProbe
}

func (g gatedAdapter) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	if err := g.probe.check(ctx); err != nil {
		return nil, err
	}
	return g.Adapter.Complete(ctx, req)
}

// friendlyModelError says what went wrong with a model call and what to do
// about it, in place of a Go dial error or an upstream JSON body.
func friendlyModelError(err error, name string, p config.ProviderConfig) string {
	where := p.BaseURL
	if where == "" {
		where = "the " + p.Type + " endpoint"
	}
	next := "Run `abhed doctor` to check the configuration, `abhed init` to write one, or set ABHED_BASE_URL to use another endpoint for this run."
	var dns *net.DNSError
	var se *model.StatusError
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Sprintf("Nothing is answering at %s (model %q). Start the model server there. %s", where, name, next)
	case errors.As(err, &dns) && dns.IsNotFound:
		return fmt.Sprintf("The host of %s does not resolve (model %q). Check base_url. %s", where, name, next)
	case errors.As(err, &se):
		switch {
		case se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden:
			key := "the key"
			if p.APIKeyEnv != "" {
				key = "the key in $" + p.APIKeyEnv
				if os.Getenv(p.APIKeyEnv) == "" {
					key = "$" + p.APIKeyEnv + ", which is not set,"
				}
			}
			return fmt.Sprintf("%s refused the request (%d): check %s and that it may use model %q. %s", where, se.Status, key, p.Model, next)
		case se.Status == http.StatusNotFound:
			return fmt.Sprintf("%s has no such model or path (404): check model %q and that base_url ends where the API starts, usually /v1. %s", where, p.Model, next)
		case se.Status == http.StatusTooManyRequests:
			return fmt.Sprintf("%s is rate limiting (429) and did not clear after %d attempts. Wait and try again, or switch with /model.", where, se.Attempts)
		case se.Status >= 500:
			return fmt.Sprintf("%s failed with %d %s after %d attempts. Try again, or switch with /model.", where, se.Status, http.StatusText(se.Status), se.Attempts)
		}
		return fmt.Sprintf("%s answered %d %s. %s", where, se.Status, http.StatusText(se.Status), next)
	case errors.As(err, &ne) && ne.Timeout():
		return fmt.Sprintf("%s did not answer in time (model %q). A local model may still be loading; try again. %s", where, name, next)
	}
	return err.Error()
}

// ---- first run ----

// firstRunNeeded reports whether this is a first run: a terminal, and no
// configuration anywhere to say which model to use.
func firstRunNeeded(workspace string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		return false
	}
	if os.Getenv("ABHED_BASE_URL") != "" || os.Getenv("ABHED_MODEL") != "" {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	for _, p := range []string{filepath.Join(home, ".abhed", "config.json"),
		filepath.Join(workspace, ".abhed", "config.json"), managed.ConfigFile} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

// ollamaBase is where a local Ollama answers: OLLAMA_HOST, as Ollama itself
// reads it, or its default.
func ollamaBase() string {
	h := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if h == "" {
		return "http://localhost:11434/v1"
	}
	if !strings.Contains(h, "://") {
		h = "http://" + h
	}
	h = strings.TrimRight(h, "/")
	if strings.HasSuffix(h, "/v1") {
		return h
	}
	return h + "/v1"
}

// listModels asks an OpenAI-compatible endpoint for its models.
func listModels(ctx context.Context, base, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &model.StatusError{Status: resp.StatusCode, Attempts: 1}
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("the answer to GET /models is not a model list")
	}
	var out []string
	for _, d := range body.Data {
		if d.ID != "" {
			out = append(out, d.ID)
		}
	}
	return out, nil
}

// probeToolCalling asks the model to call a tool, through the adapter a
// session would use, and reports whether it did.
func probeToolCalling(ctx context.Context, p config.ProviderConfig) (bool, error) {
	a, err := p.Adapter()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ch, err := a.Complete(ctx, model.Request{
		System:   "You are testing tool calling. Call the tool you are given; do not answer in text.",
		Messages: []model.Message{{Role: model.RoleUser, Content: "Call get_time with zone UTC."}},
		Tools: []model.ToolDef{{Name: "get_time", Description: "Returns the current time in a zone.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"zone":{"type":"string"}},"required":["zone"]}`)}},
		MaxTokens: 256,
	})
	if err != nil {
		return false, err
	}
	called := false
	for c := range ch {
		switch c.Type {
		case model.ChunkToolCall:
			called = called || (c.ToolCall != nil && c.ToolCall.Name == "get_time")
		case model.ChunkError:
			return false, c.Err
		}
	}
	return called, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// onboarding is the first-run conversation, on plain lines before the
// session takes the terminal.
type onboarding struct {
	in  io.Reader
	out io.Writer
	ctx context.Context
}

func (o onboarding) ask(q, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(o.out, "%s [%s]: ", q, def)
	} else {
		fmt.Fprintf(o.out, "%s: ", q)
	}
	line, err := readAnswer(o.in)
	if err != nil {
		fmt.Fprintln(o.out)
		return "", err
	}
	if line = strings.TrimSpace(line); line == "" {
		return def, nil
	}
	return line, nil
}

// firstRun offers to write the person's own configuration: a local Ollama
// model if one answers, or an OpenAI-compatible endpoint by URL and the
// name of the variable that holds its key. A key itself is never written.
// It writes only ~/.abhed/config.json, and only when asked to.
func firstRun(ctx context.Context, in io.Reader, out io.Writer) error {
	o := onboarding{in: in, out: out, ctx: ctx}
	fmt.Fprintln(out, "\nWelcome to Abhed. There is no configuration yet, so first choose the model it talks to.")
	fmt.Fprintln(out, "Nothing is written unless you confirm; `abhed init` writes a workspace configuration instead.")

	base := ollamaBase()
	fmt.Fprintf(out, "\nLooking for Ollama at %s … ", strings.TrimSuffix(base, "/v1"))
	models, err := listModels(ctx, base, "")
	switch {
	case err != nil:
		fmt.Fprintln(out, "not running.")
	case len(models) == 0:
		fmt.Fprintln(out, "running, with no models pulled (`ollama pull <model>` fetches one).")
	default:
		fmt.Fprintf(out, "running, with %d model(s).\n", len(models))
	}
	fmt.Fprintln(out)
	var choices []string
	for i, m := range models {
		if i == 9 {
			break
		}
		choices = append(choices, m)
		fmt.Fprintf(out, "  %d) Ollama: %s\n", i+1, config.Printable(m))
	}
	fmt.Fprintln(out, "  e) an OpenAI-compatible endpoint: its URL, and the name of the variable holding its key")
	fmt.Fprintln(out, "  s) skip: start with the defaults and write nothing")
	fmt.Fprintln(out, "  (Other provider types, such as the hosted APIs, are listed by `abhed providers`.)")
	def := "e"
	if len(choices) > 0 {
		def = "1"
	}

	var name string
	var p config.ProviderConfig
	for p.BaseURL == "" {
		pick, err := o.ask("\nChoose", def)
		if err != nil {
			return err
		}
		switch pick = strings.ToLower(pick); pick {
		case "s":
			fmt.Fprintln(out, "Nothing written. The defaults expect Ollama at localhost:11434.")
			return nil
		case "e":
			if name, p, err = o.endpoint(); err != nil {
				return err
			}
		default:
			n, convErr := strconv.Atoi(pick)
			if convErr != nil || n < 1 || n > len(choices) {
				fmt.Fprintf(out, "  %q is not one of the choices.\n", pick)
				continue
			}
			name, p = "ollama", config.ProviderConfig{Type: "openai-compatible", BaseURL: base, Model: choices[n-1]}
		}
	}

	fmt.Fprintf(out, "\nChecking that %s can call tools (a model that is loading can take a while) … ", config.Printable(p.Model))
	pk := p
	if pk.APIKeyEnv != "" {
		pk.APIKey = os.Getenv(pk.APIKeyEnv)
	}
	switch called, err := probeToolCalling(ctx, pk); {
	case err != nil:
		fmt.Fprintf(out, "it did not answer.\n  %s\n", friendlyModelError(err, name, p))
	case called:
		fmt.Fprintln(out, "it can.")
	default:
		fmt.Fprintln(out, "it answered without calling the tool. Abhed works through tools, so this model will do little; a larger or tool-trained one is better.")
	}

	auto := false
	ans, err := o.ask("\nLet the agent keep memory notes of its own between sessions? They are recorded, and text a file planted could persist in them (y/N)", "n")
	if err != nil {
		return err
	}
	auto = strings.HasPrefix(strings.ToLower(ans), "y")

	ans, err = o.ask("Write this to ~/.abhed/config.json? (Y/n)", "y")
	if err != nil {
		return err
	}
	if strings.HasPrefix(strings.ToLower(ans), "n") {
		fmt.Fprintln(out, "Nothing written.")
		return nil
	}
	path, err := writeUserConfig(name, p, auto)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote %s. `abhed doctor` checks it at any time.\n\n", config.Printable(path))
	return nil
}

// endpoint asks for an OpenAI-compatible endpoint.
func (o onboarding) endpoint() (string, config.ProviderConfig, error) {
	var p config.ProviderConfig
	p.Type = "openai-compatible"
	for {
		u, err := o.ask("Base URL, such as http://localhost:8000/v1", "")
		if err != nil {
			return "", p, err
		}
		pu, perr := url.Parse(u)
		if perr != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			fmt.Fprintln(o.out, "  That is not an http(s) URL.")
			continue
		}
		p.BaseURL = strings.TrimRight(u, "/")
		if p.APIKeyEnv, err = o.keyName(); err != nil {
			return "", p, err
		}
		if p.APIKeyEnv == "" || os.Getenv(p.APIKeyEnv) == "" || pu.Scheme != "http" || hostedLabel(p.BaseURL) == "local" {
			break
		}
		// Plain http to another machine would carry the key readable on the way.
		ok, err := o.confirm(fmt.Sprintf("  %s is plain http on another machine, so the key would be sent unencrypted. Send it anyway? (y/N)", config.PrintableURL(p.BaseURL)))
		if err != nil {
			return "", p, err
		}
		if ok {
			break
		}
		fmt.Fprintln(o.out, "  Enter an https:// URL, or one on this machine.")
	}
	models, err := listModels(o.ctx, p.BaseURL, os.Getenv(p.APIKeyEnv))
	if err != nil {
		fmt.Fprintf(o.out, "  Could not list its models: %s\n", friendlyModelError(err, "endpoint", p))
	} else if len(models) > 0 {
		shown := make([]string, 0, 12)
		for _, m := range models[:min(len(models), 12)] {
			shown = append(shown, config.Printable(m))
		}
		fmt.Fprintf(o.out, "  It offers: %s\n", strings.Join(shown, ", "))
	}
	def := ""
	if len(models) == 1 {
		def = models[0]
	}
	for p.Model == "" {
		m, err := o.ask("Model name", def)
		if err != nil {
			return "", p, err
		}
		p.Model = m
	}
	return "endpoint", p, nil
}

// keyName asks for the name of the variable holding the key. What looks
// like a key is refused and never echoed, and a name that is not set in
// this shell is taken only on a yes.
func (o onboarding) keyName() (string, error) {
	for {
		v, err := o.ask("Name of the environment variable holding its key (blank for none)", "")
		if err != nil || v == "" {
			return "", err
		}
		if looksLikeKey(v) {
			fmt.Fprintln(o.out, "  That looks like a key, not a variable name, so it was not kept. Put the key in a variable, such as MY_ENDPOINT_KEY, and enter that NAME; the key itself is never written.")
			continue
		}
		if !envName.MatchString(v) {
			fmt.Fprintln(o.out, "  That is not a variable name. Enter the NAME of the variable, such as MY_ENDPOINT_KEY; the key itself is never written.")
			continue
		}
		if os.Getenv(v) != "" {
			return v, nil
		}
		ok, err := o.confirm(fmt.Sprintf("  $%s is not set in this shell. Use that name anyway, and set it before starting Abhed? (y/N)", v))
		if err != nil {
			return "", err
		}
		if ok {
			return v, nil
		}
	}
}

// confirm asks a yes-or-no question whose default is no.
func (o onboarding) confirm(q string) (bool, error) {
	ans, err := o.ask(q, "n")
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(strings.ToLower(ans), "y"), nil
}

// keyPrefixes start the keys of well-known services; no variable name does.
var keyPrefixes = []string{"sk-", "sk_", "hf_", "gsk_", "AIza", "xai-", "pplx-", "ghp_", "gho_", "ghu_", "ghs_", "ghr_",
	"github_pat_", "glpat-", "lsv2_", "tvly-", "pa-", "jina_", "xoxb-", "xoxp-", "AKIA", "ASIA", "r8_", "nvapi-", "pk_live_", "sk_live_", "rk_live_", "Bearer "}

// looksLikeKey reports whether an answer to "which variable" is more likely
// a pasted key: a known key prefix, or a long run of mixed characters
// unlike an UPPER_SNAKE name.
func looksLikeKey(v string) bool {
	for _, p := range keyPrefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	var lower, upper, digit, under int
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
			lower++
		case r >= 'A' && r <= 'Z':
			upper++
		case r >= '0' && r <= '9':
			digit++
		case r == '_':
			under++
		}
	}
	n := len(v)
	switch {
	case n >= 20 && lower > 0 && upper > 0 && digit > 0:
		return true
	case n >= 24 && under == 0 && digit >= 4:
		return true
	case n >= 32 && under == 0:
		return true
	}
	return false
}

// writeUserConfig writes the person's own configuration, which must not
// exist yet. It holds a key's variable name, never a key.
func writeUserConfig(name string, p config.ProviderConfig, autoMemory bool) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	prov := map[string]any{"type": p.Type, "base_url": p.BaseURL, "model": p.Model}
	if p.APIKeyEnv != "" {
		prov["api_key_env"] = p.APIKeyEnv
	}
	cfg := map[string]any{
		"model": map[string]any{"default": name, "providers": map[string]any{name: prov}},
	}
	// Off is the default, so only a yes is written.
	if autoMemory {
		cfg["memory"] = map[string]any{"auto": true}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".abhed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "config.json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the person's own ~/.abhed/config.json, created new
	if err != nil {
		return path, err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return path, err
	}
	return path, f.Close()
}
