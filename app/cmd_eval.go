package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/eval"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/webfetch"
)

// evalAllowed refuses an eval under a managed configuration: it approves
// every prompt with nobody to ask, which is more than bypass.
func evalAllowed(cfg config.Config) error {
	if cfg.Managed {
		return fmt.Errorf("eval approves every prompt with nobody to ask, which is refused "+
			"under the managed configuration %s; run it where there is none", managed.ConfigFile)
	}
	return nil
}

// evalAllow lets corpora that compile and test code do so unattended.
var evalAllow = []string{"bash(go *)", "bash(npm *)", "bash(python *)", "bash(cat *)", "bash(ls*)"}

// evalCmd runs the evaluation corpus against the configured model.
//
// Per docs P1 the harness is the dominant variable in agent success, so this is
// how a harness change is judged. Per P10 the report carries behavioural flags
// alongside the score, because identical pass rates hide different behaviour.
func evalCmd(workspace, corpusDir, jsonPath string, trust config.TrustChoice) int {
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust})
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	if err := evalAllowed(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	if err := vaultLoads(); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	provider, err := cfg.Provider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}

	tasks, err := eval.LoadTasks(corpusDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	fmt.Printf("running %d tasks against %s\n\n", len(tasks), provider.Model)

	adapter := buildAdapter(provider)
	workRoot, err := os.MkdirTemp("", "abhed-eval-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(workRoot) }()

	runner := func(ctx context.Context, ws string, task eval.Task) ([]agent.Event, eval.Result, error) {
		sb, err := buildSandbox(cfg, ws)
		if err != nil {
			return nil, eval.Result{}, err
		}
		sess, err := tools.NewSession(ws)
		if err != nil {
			return nil, eval.Result{}, err
		}
		if sess.Syntax, err = tools.ParseSyntaxMode(cfg.Tools.SyntaxCheck); err != nil {
			return nil, eval.Result{}, err
		}
		vault := openVault()
		// Skills, the web tools, the todo list and subagents are part of the
		// agent under test, not extras: a corpus exercising one would
		// otherwise measure an agent that never had it. MCP servers, corpora,
		// clusters, hosts, the index and extensions are left out, so a score
		// depends on the harness and the task and not on what those reach.
		set := toolset.Build(ctx, cfg, toolset.Options{
			Workspace: ws,
			Bash: tools.Bash{Sandbox: sb.Command, Secrets: vault.Env, SecretNames: vaultNames(vault),
				Isolation: tools.Isolation{Tier: string(sb.Tier()), Network: cfg.Sandbox.AllowNetwork}},
			Parts: toolset.Skills | toolset.WebSearch | toolset.WebFetch,
			Vault: vault,
		})
		defer set.Close()
		if err := grantDirs(sess, config.Config{}, set.SkillDirs()); err != nil {
			return nil, eval.Result{}, err
		}

		pol := policy.New(policy.ModeAuto)
		pol.AskReadOnly = webfetch.AskReadOnly(cfg.WebFetch.Enabled, cfg.WebFetch.AllowedHosts)
		pol.Roots = sess.PolicyRoots
		must(pol.AddDeny(cfg.Permissions.Deny...))
		// The operator's own allow rules apply, so an eval run is governed the
		// same way a real session is. The build-tool defaults stay for corpora
		// that compile and test code.
		must(pol.AddAllow(cfg.Permissions.Allow...))
		must(pol.AddAllow(evalAllow...))

		store := agent.NewMemStore()
		sessionID := "eval-" + task.ID
		rec := agent.NewRecorder(store, sessionID, "")
		rec.Redact = vault.Redactor()

		loopCfg := agent.DefaultConfig()
		if task.MaxTurns > 0 {
			loopCfg.MaxTurns = task.MaxTurns
		} else {
			loopCfg.MaxTurns = 30
		}

		budget := toolset.Budget(cfg)
		factory := &agent.SubagentFactory{Adapter: adapter, Policy: pol, Session: sess, Store: store,
			Budget: budget, Config: loopCfg, Workspace: ws, Redact: vault.Redactor(), Definitions: set.Agents}
		registry := toolset.Subagents(set.Registry, factory, cfg.Limits.MaxParallelSubagents)
		loopCfg.SystemPrompt = toolset.SystemPrompt(ws, adapter, set.SkillListing, registry.Names())

		loop := agent.NewLoop(adapter, registry, pol, agent.AutoApprove{Yes: true}, sess, rec, loopCfg)
		loop.Budget = budget
		loop.Compactor = agent.NewCompactor(adapter, loopCfg.CompactAt)

		runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()

		reason, runErr := loop.Run(runCtx, task.Prompt)
		usage := loop.Usage()
		events, _ := store.Events(sessionID)

		return events, eval.Result{
			Turns: usage.Turns, TokensIn: usage.InputTokens, TokensOut: usage.OutputTokens,
			CacheHitRate: usage.CacheHitRate(), Compactions: usage.Compactions,
			Terminal: string(reason),
		}, runErr
	}

	stopper := cancelOnStop(stopReturns)
	defer stopper.stop()
	results, err := eval.Run(stopper.ctx, tasks, workRoot, runner)
	// A stopped run is not a result: tasks it cut short would read as passed or failed.
	if code, stopped := stopCode(stopper.ctx); stopped {
		fmt.Fprintf(os.Stderr, "abhed: eval %s after %d of %d tasks; no summary or report written\n",
			context.Cause(stopper.ctx), len(results), len(tasks))
		return code
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}

	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
		}
		fmt.Printf("  %-4s %-28s %2d turns  %6d tok\n", status, r.TaskID, r.Turns, r.TokensIn)
		for _, f := range r.Flags {
			marker := "flag"
			if f.Blocking {
				marker = "BLOCK"
			}
			fmt.Printf("       %s %s: %s\n", marker, f.Kind, f.Detail)
		}
		for _, f := range r.Failures {
			fmt.Printf("       %s\n", f)
		}
	}

	summary := eval.Summarize(results, provider.Model)
	fmt.Printf("\n%s", summary.Render())

	if jsonPath != "" {
		data, _ := json.MarshalIndent(summary, "", "  ")
		if err := os.WriteFile(jsonPath, append(data, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: writing %s: %v\n", jsonPath, err)
			return 1
		}
		fmt.Printf("\nreport written to %s\n", jsonPath)
	}

	// A non-zero exit lets CI gate on the eval, which is the point.
	if summary.Passed < summary.Tasks {
		return 1
	}
	return 0
}
