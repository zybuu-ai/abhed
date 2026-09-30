package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/server"
)

func runOnce(ctx context.Context, store server.EventStore, r *ui.Renderer, jsonOut bool,
	adapter model.Adapter, registry *tools.Registry, pol *policy.Engine,
	approver agent.Approver, sess *tools.Session, cfg agent.Config,
	appCfg config.Config, prompt string, budget *agent.Budget, extHost *extension.Host) int {

	// A new session, or with -c or -r the recorded one it goes on with.
	sessionID, seed, err := headlessSession(ctx, &cliState{store: store, appCfg: appCfg, workspace: sess.Root, adapter: adapter})
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1 // a run with no session row would write into another's record
	}
	rec := agent.NewRecorder(store, sessionID, "")
	rec.Redact = openVault().Redactor()

	events := store.Subscribe(sessionID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			if jsonOut {
				b, _ := json.Marshal(ev)
				fmt.Println(string(b))
			} else {
				r.Event(ev)
			}
		}
	}()

	loop := agent.NewLoop(adapter, registry, pol, approver, sess, rec, cfg)
	loop.Provider = appCfg.Model.Default
	// The factory's budget, so the subagents' spend and the loop's are one.
	loop.Budget = budget
	seed(loop)
	if startFlags.Name != "" {
		_, _ = loop.Recorder.Record(agent.EvSessionNamed, agent.ActorUser, agent.Trusted, agent.SessionNamed{Name: startFlags.Name})
	}
	// Nobody comes back to a -p run, so its background tasks are joined: the
	// run, and the exit code, wait for them.
	agent.NewBackground(loop, toolset.BackgroundPolicy(appCfg, agent.WakeOff))
	defer loop.Background.Close(agent.TermSessionClosed)
	if appCfg.Sets("subagents.wake") && appCfg.Subagents.Wake != "off" {
		fmt.Fprintf(os.Stderr, "abhed: note: subagents.wake is %s, but -p runs background tasks joined: it waits for them\n", config.Printable(appCfg.Subagents.Wake))
	}
	loop.Compactor = agent.NewCompactor(adapter, cfg.CompactAt)
	toolset.Summarize(loop.Compactor, extHost, sessionID)
	reason, err := loop.Run(ctx, prompt)

	store.Unsubscribe(sessionID, events)
	<-done

	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		noteIgnoredModel(appCfg)
		return agent.TermError.ExitCode()
	}
	if !jsonOut {
		printUsage(r, loop.Usage())
	}
	if reason.ExitCode() != 0 {
		noteIgnoredModel(appCfg)
	}
	return reason.ExitCode()
}

func printUsage(r *ui.Renderer, u agent.Usage) {
	s := r.Style()
	line := fmt.Sprintf("%d turns · %d in / %d out tokens", u.Turns, u.InputTokens, u.OutputTokens)
	// Cache hit rate is a UX metric as much as a capacity one (docs P8), so it
	// is shown rather than hidden in telemetry.
	if u.InputTokens > 0 && u.CachedTokens > 0 {
		line += fmt.Sprintf(" · %.0f%% cached (%.1fx prefill)", u.CacheHitRate()*100, u.PrefillSavings())
	}
	if u.Compactions > 0 {
		line += fmt.Sprintf(" · %d compaction(s)", u.Compactions)
	}
	fmt.Printf("%s\n", s.Dim(line))
}
