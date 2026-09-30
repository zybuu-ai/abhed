package app

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// slashModel is /model.
func slashModel(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	if len(fields) < 2 {
		fmt.Printf("  current: %s (%s)\n", st.adapter.Profile().Name, st.appCfg.Model.Default)
		names := make([]string, 0, len(st.appCfg.Model.Providers))
		for name := range st.appCfg.Model.Providers {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Printf("  configured providers: %s\n", strings.Join(names, ", "))
		return false
	}
	if _, found := st.appCfg.Model.Providers[fields[1]]; !found {
		fmt.Printf("  %s no provider %q in config\n", s.Red("✕"), fields[1])
		return false
	}
	// Resolve through ProviderNamed so the selected provider's api_key_env
	// is read into APIKey. A raw map lookup skips that step, so any
	// provider other than the default (whose key applyEnv injects) would
	// build an adapter with no credential and fail the first call with 401.
	p, resolveErr := st.appCfg.ProviderNamed(fields[1])
	if resolveErr != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), resolveErr)
		return false
	}
	next, buildErr := newAdapter(p)
	if buildErr != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), buildErr)
		return false
	}
	if st.loop != nil {
		// Recorded before it takes effect, so the record names the model that answers.
		release, claimErr := claimForWrite(ctx, st)
		if claimErr != nil {
			fmt.Printf("  %s model not switched: %v\n", s.Red("✕"), claimErr)
			return false
		}
		switchErr := st.loop.SwitchModel(fields[1], next)
		release()
		if switchErr != nil {
			fmt.Printf("  %s model not switched: the switch could not be recorded: %v\n", s.Red("✕"), switchErr)
			return false
		}
	}
	st.appCfg.Model.Default = fields[1]
	st.provider = p
	st.adapter = next
	st.moved = nil // the switch just recorded says where the session is
	fmt.Printf("  %s\n", s.Dim("switched to "+next.Profile().Name+" — the conversation is kept"))
	fmt.Printf("  %s\n", s.Dim("(the next turn re-prefills: the new provider has not seen this prefix)"))
	return false
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
