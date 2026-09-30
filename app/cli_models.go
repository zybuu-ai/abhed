package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/model", Args: "[name]", Help: "show or switch the model, keeping the conversation", Group: "model", Order: 100, Run: slashModel})
	registerSlash(slashCmd{Name: "/effort", Args: "[low|medium|high|on|off|default]", Help: "how hard the model reasons, where it supports it", Group: "model", Order: 102, Run: slashEffort})
}

// modelItems are the models /model offers: the configured providers a
// session may use, never an endpoint typed at the prompt.
func modelItems(st *cliState) []ui.PickItem {
	var items []ui.PickItem
	for _, name := range toolset.OfferedModels(st.appCfg) {
		p := st.appCfg.Model.Providers[name]
		detail := []string{p.Model}
		if p.ContextWindow > 0 {
			detail = append(detail, fmt.Sprintf("%dk context", p.ContextWindow/1024))
		}
		detail = append(detail, hostedLabel(p.BaseURL))
		if name == st.appCfg.Model.Default {
			detail = append(detail, "current")
		}
		items = append(items, ui.PickItem{ID: name, Label: name, Detail: strings.Join(detail, " · ")})
	}
	return items
}

// hostedLabel says whether an endpoint is on this machine.
func hostedLabel(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return "hosted"
	}
	h := u.Hostname()
	if ip := net.ParseIP(h); h == "localhost" || (ip != nil && ip.IsLoopback()) {
		return "local"
	}
	return "hosted"
}

// slashModel is /model: a pick of the configured models, or a switch by name.
func slashModel(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	st := e.st
	name := ""
	if len(args) > 0 {
		name = args[0]
	} else {
		title := fmt.Sprintf("current: %s (%s)", st.adapter.Profile().Name, st.appCfg.Model.Default)
		picked, err := e.ui.Pick(ctx, ui.PickSpec{Title: title, Items: modelItems(st), Default: st.appCfg.Model.Default})
		if errors.Is(err, ui.ErrNoAnswer) {
			e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "unchanged; /model <name> switches"})
			return false, nil
		}
		if err != nil {
			return false, err
		}
		name = picked
	}
	if name == st.appCfg.Model.Default {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "already on " + name})
		return false, nil
	}
	return false, switchModel(ctx, e, name)
}

// switchModel moves the session to a configured provider, recorded first.
func switchModel(ctx context.Context, e *cmdEnv, name string) error {
	st := e.st
	if st.appCfg.ManagedSets("model.default") {
		return fmt.Errorf("the managed configuration sets the model to %s; it is not switched", st.appCfg.Model.Default)
	}
	if !slices.Contains(toolset.OfferedModels(st.appCfg), name) {
		return fmt.Errorf("no provider %q in config; /model lists them", name)
	}
	// Resolve through ProviderNamed so the selected provider's api_key_env
	// is read into APIKey. A raw map lookup skips that step, so any
	// provider other than the default (whose key applyEnv injects) would
	// build an adapter with no credential and fail the first call with 401.
	p, err := st.appCfg.ProviderNamed(name)
	if err != nil {
		return err
	}
	next, err := newAdapter(p)
	if err != nil {
		return err
	}
	if st.loop != nil {
		// Recorded before it takes effect, so the record names the model that answers.
		release, claimErr := claimForWrite(ctx, st)
		if claimErr != nil {
			return fmt.Errorf("model not switched: %w", claimErr)
		}
		switchErr := st.loop.SwitchModel(name, next)
		release()
		if switchErr != nil {
			return fmt.Errorf("model not switched: the switch could not be recorded: %w", switchErr)
		}
	}
	st.appCfg.Model.Default = name
	st.provider = p
	st.adapter = next
	st.moved = nil // the switch just recorded says where the session is
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "switched to " + next.Profile().Name + " — the conversation is kept"})
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "(the next turn re-prefills: the new provider has not seen this prefix)"})
	return nil
}

// slashEffort is /effort: the reasoning effort, or thinking on and off,
// where the model's provider supports it.
func slashEffort(_ context.Context, e *cmdEnv, args []string) (bool, error) {
	st := e.st
	p := st.provider
	sampling := st.adapter.Profile().Sampling
	if len(args) == 0 {
		cur := "default"
		switch {
		case p.Params.Effort != "":
			cur = p.Params.Effort
		case p.Params.Think != nil && *p.Params.Think:
			cur = "on"
		case p.Params.Think != nil:
			cur = "off"
		}
		var can []string
		if sampling.Effort {
			can = append(can, "low, medium, high")
		}
		if sampling.Think {
			can = append(can, "on, off")
		}
		if len(can) == 0 {
			e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "effort " + cur + "; " + st.appCfg.Model.Default + " offers no reasoning control"})
			return false, nil
		}
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "effort " + cur + " (" + strings.Join(can, "; ") + ", or default)"})
		return false, nil
	}
	arg := strings.ToLower(args[0])
	switch arg {
	case "low", "medium", "high", "max", "xhigh":
		if !sampling.Effort {
			return false, fmt.Errorf("%s does not take a reasoning effort", st.appCfg.Model.Default)
		}
		if arg == "max" || arg == "xhigh" {
			arg = "high"
		}
		p.Params.Effort = arg
	case "on", "off":
		if !sampling.Think {
			return false, fmt.Errorf("%s has no thinking switch", st.appCfg.Model.Default)
		}
		on := arg == "on"
		p.Params.Think = &on
	case "default":
		p.Params.Effort, p.Params.Think = "", nil
	default:
		return false, fmt.Errorf("unknown effort %q; use low, medium, high, on, off or default", args[0])
	}
	next, err := newAdapter(p)
	if err != nil {
		return false, err
	}
	st.provider, st.adapter = p, next
	if st.loop != nil {
		st.loop.SetAdapter(next)
	}
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "effort " + arg + " from the next turn"})
	return false, nil
}

// buildAdapter constructs the configured provider.
//
// Every provider is reached through the registry in internal/model, so adding
// one is a new file with an init rather than another branch here. A build
// failure is fatal by design: a mistyped provider type or an unsupported
// sampling parameter is a configuration error, and discovering it now beats
// discovering it on the first model call of a long session.
func buildAdapter(p config.ProviderConfig) model.Adapter {
	a, err := newAdapter(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "model: %v\n", err)
		os.Exit(1)
	}
	return a
}

// newAdapter builds a provider's adapter, reporting its retries on stderr.
func newAdapter(p config.ProviderConfig) (model.Adapter, error) {
	a, err := p.Adapter()
	if err != nil {
		return nil, err
	}
	// A retry is silence from the user's point of view, and silence in an
	// interactive session is indistinguishable from a hang. Say what happened.
	type notifier interface{ SetNotify(func(string)) }
	if n, ok := a.(notifier); ok {
		n.SetNotify(func(msg string) {
			fmt.Fprintf(os.Stderr, "  %s\n", msg)
		})
	}
	return a, nil
}

// fallbackChain is the providers a session may move to when its model is
// unavailable: -fallback-model then model.fallback, in order, each an
// offered configured provider other than the default. A managed
// model.default is not left unless the managed configuration names the
// fallbacks too. The second result are warnings about names left out.
func fallbackChain(cfg config.Config, flag string) ([]string, []string) {
	names := append(splitRules(flag), cfg.Model.Fallback...)
	if len(names) == 0 {
		return nil, nil
	}
	if cfg.ManagedSets("model.default") && !cfg.ManagedSets("model.fallback") {
		return nil, []string{"the managed configuration sets model.default, so no fallback is used"}
	}
	offered := toolset.OfferedModels(cfg)
	var chain, warn []string
	for _, n := range names {
		switch {
		case n == cfg.Model.Default || slices.Contains(chain, n):
		case !slices.Contains(offered, n):
			warn = append(warn, fmt.Sprintf("fallback %q is not a configured provider; left out", n))
		default:
			chain = append(chain, n)
		}
	}
	return chain, warn
}

// fallbackAdapter moves to the next model in its chain when the current one
// is unreachable or refuses access, and records the move. It never moves
// back within a session.
type fallbackAdapter struct {
	mu     sync.Mutex
	names  []string // the default, then the fallbacks
	cur    int
	built  []model.Adapter
	build  func(name string) (model.Adapter, error)
	record func(from, to, reason string)
}

func newFallbackAdapter(primaryName string, primary model.Adapter, chain []string, build func(string) (model.Adapter, error)) *fallbackAdapter {
	f := &fallbackAdapter{names: append([]string{primaryName}, chain...), build: build}
	f.built = make([]model.Adapter, len(f.names))
	f.built[0] = primary
	return f
}

// SetRecord binds where a move is recorded: the current conversation.
func (f *fallbackAdapter) SetRecord(r func(from, to, reason string)) {
	f.mu.Lock()
	f.record = r
	f.mu.Unlock()
}

func (f *fallbackAdapter) current() model.Adapter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.built[f.cur]
}

// Current is the configured name of the model now answering.
func (f *fallbackAdapter) Current() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.names[f.cur]
}

func (f *fallbackAdapter) Name() string           { return f.current().Name() }
func (f *fallbackAdapter) Profile() model.Profile { return f.current().Profile() }
func (f *fallbackAdapter) CountTokens(req model.Request) (int, error) {
	return f.current().CountTokens(req)
}

func (f *fallbackAdapter) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	for {
		f.mu.Lock()
		i, a := f.cur, f.built[f.cur]
		f.mu.Unlock()
		ch, err := a.Complete(ctx, req)
		if err == nil || ctx.Err() != nil {
			return ch, err
		}
		why, move := fallbackReason(err)
		if !move || i+1 >= len(f.names) {
			return nil, err
		}
		next, berr := f.build(f.names[i+1])
		if berr != nil {
			return nil, fmt.Errorf("%w (and fallback %s could not be built: %v)", err, f.names[i+1], berr)
		}
		f.mu.Lock()
		if f.cur == i {
			f.cur, f.built[i+1] = i+1, next
		}
		record := f.record
		f.mu.Unlock()
		fmt.Fprintf(os.Stderr, "  model %s is %s; moving to %s\n", f.names[i], why, f.names[i+1])
		if record != nil {
			record(f.names[i], f.names[i+1], why)
		}
	}
}

// fallbackReason says whether an error is the model being unavailable to
// this session, as opposed to a request it rejected.
func fallbackReason(err error) (string, bool) {
	var dns *net.DNSError
	var se *model.StatusError
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "unreachable", true
	case errors.As(err, &dns) && dns.IsNotFound:
		return "unreachable", true
	case errors.As(err, &se):
		switch {
		case se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden:
			return fmt.Sprintf("refusing access (%d)", se.Status), true
		case se.Status == http.StatusNotFound:
			return "not found (404)", true
		case se.Status >= 500 || se.Status == http.StatusTooManyRequests:
			return fmt.Sprintf("unavailable (%d)", se.Status), true
		}
	}
	return "", false
}

// recordFallback is the record side of a move.
func recordFallback(rec *agent.Recorder) func(from, to, reason string) {
	return func(from, to, reason string) {
		if _, err := rec.Record(agent.EvModelFallback, agent.ActorSystem, agent.Trusted,
			agent.ModelFallback{From: from, To: to, Reason: reason}); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: recording the fallback: %v\n", err)
		}
	}
}

// providersCmd lists the model providers this build supports.
//
// The set is whatever registered itself at init, so it is accurate for the
// binary in hand rather than for the documentation — which matters for a build
// that deliberately drops the cloud providers for an air-gapped install.
func providersCmd() int {
	fmt.Println("Model providers in this build:")
	fmt.Println()
	for _, d := range model.Describe() {
		fmt.Println("  " + d)
	}
	fmt.Println()
	fmt.Println("Set one as \"type\" in .abhed/config.json under model.providers.")
	fmt.Println("Sampling parameters go in that provider's \"params\" object;")
	fmt.Println("a parameter the provider cannot honour is reported at startup")
	fmt.Println("rather than silently ignored.")
	return 0
}
