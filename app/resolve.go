package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/forge"
	"github.com/zybuu-ai/abhed/internal/policy"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// The forge token never enters the session: the issue is read and the pull
// request opened here, outside the sandbox, and the run itself sees only
// the issue text. Opening the request is judged by policy as its own
// mutating action, forge_pr, so no mode approves it on its own.

var newForge = forge.New

// newResolveRunner runs the agent in the worktree. A variable so the test can
// stand in for the model.
var newResolveRunner = func(ctx context.Context, opts abhed.Options) (forge.Runner, error) {
	a, err := abhed.New(ctx, opts)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, _ string, prompt string) error {
		defer a.Close()
		_, err := a.Run(ctx, prompt)
		return err
	}, nil
}

// pushWork sends the branch to the issue's repository. A variable so the test
// can push to a local repository instead of a forge.
var pushWork = func(ctx context.Context, w *forge.Work, repo string, ref forge.Ref, auth, ca string) error {
	return w.Push(ctx, repo, ref, auth, ca)
}

// resolveMode is the run's mode. The default, auto, yields to a mode the
// managed configuration pins; a mode asked for by flag is judged as given.
func resolveMode(cfg config.Config, fs *flag.FlagSet, mode string) string {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "mode" })
	if !set && cfg.ManagedSets("permissions.mode") {
		return ""
	}
	return mode
}

func resolveCmd(workspace string, args []string) int {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	kind := fs.String("kind", "", "github, gitlab or gitea; inferred from the host when empty")
	base := fs.String("base", "", "branch the pull request targets (default: the repository's default branch)")
	remote := fs.String("remote", "", "ignored: the branch is pushed to the issue's repository")
	ca := fs.String("ca", os.Getenv("ABHED_FORGE_CA"), "PEM file with the certificate authority of a self-hosted forge")
	mode := fs.String("mode", "auto", "permission mode for the run; a mode the managed configuration pins replaces the default")
	allow := fs.String("allow", "", "comma-separated allow rules for the run, e.g. 'bash(go test*)'")
	yes := fs.Bool("y", false, "open the pull request without asking (an allow rule forge_pr(*) does the same)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: abhed resolve [flags] <issue-url>\n"+
			"  Reads the issue, works on it in a branch in its own worktree, commits, pushes,\n"+
			"  and opens a pull request that links the issue. GitHub, GitLab and Gitea/Forgejo,\n"+
			"  self-hosted included. Token: GITHUB_TOKEN / GITLAB_TOKEN / GITEA_TOKEN, or the\n"+
			"  same name in `abhed secret`.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	stopper := cancelOnStop(stopReturns)
	defer stopper.stop()
	ctx := stopper.ctx
	// kept is set when the worktree is left for inspection: the run failed, or
	// resolve was stopped once it existed.
	var worktree string
	kept := false
	fail := func(err error) int {
		// Stopped by a signal: exit as a shell reports one, 128 plus its number.
		if code, stopped := stopCode(ctx); stopped {
			fmt.Fprintf(os.Stderr, "abhed: resolve %s: %v\n", context.Cause(ctx), err)
			if worktree != "" && !kept {
				kept = true
				fmt.Fprintf(os.Stderr, "abhed: the worktree %s is kept for inspection\n", worktree)
			}
			return code
		}
		fmt.Fprintf(os.Stderr, "abhed: resolve: %v\n", err)
		return 1
	}

	ref, err := forge.Parse(fs.Arg(0))
	if err != nil {
		return fail(err)
	}
	cfg, err := config.Load(workspace)
	if err != nil {
		return fail(err)
	}
	*mode = resolveMode(cfg, fs, *mode)
	opts := forge.Options{Kind: *kind, CAFile: *ca}
	if opts.Kind != "" {
		opts.Token = tokenFor(opts.Kind)
	}
	fg, err := newForge(ctx, ref, opts)
	if err != nil && opts.Token == "" && opts.Kind == "" {
		// The kind was inferred and the environment had no token: try the store.
		for _, k := range []string{forge.KindGitHub, forge.KindGitLab, forge.KindGitea} {
			if strings.Contains(err.Error(), strings.ToUpper(k)+"_TOKEN") {
				opts.Kind, opts.Token = k, tokenFor(k)
				fg, err = newForge(ctx, ref, opts)
				break
			}
		}
	}
	if err != nil {
		return fail(err)
	}

	is, err := fg.Issue(ctx, ref)
	if err != nil {
		return fail(err)
	}
	target := *base
	if target == "" {
		if target, err = fg.DefaultBranch(ctx, ref); err != nil {
			return fail(err)
		}
	}
	fmt.Fprintf(os.Stderr, "abhed: %s — %s\n", ref, is.Title)

	work, err := forge.Begin(ctx, workspace, is, target)
	if err != nil {
		return fail(err)
	}
	worktree = work.Dir
	defer func() {
		if !kept {
			work.Cleanup(context.WithoutCancel(ctx), workspace)
		}
	}()

	runner, err := newResolveRunner(ctx, abhed.Options{
		Workspace: work.Dir, ConfigDir: workspace, Mode: *mode,
		Allow: splitRules(*allow), Sandbox: true,
		OnEvent: func(ev abhed.Event) {
			if ev.Type == "agent.message" {
				var p struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(ev.Payload, &p)
				fmt.Fprintln(os.Stderr, strings.TrimSpace(p.Text))
			}
		},
	})
	if err != nil {
		return fail(err)
	}
	if err := runner(ctx, work.Dir, forge.Prompt(is)); err != nil || ctx.Err() != nil {
		if err == nil {
			err = context.Cause(ctx)
		}
		kept = true
		return fail(fmt.Errorf("the run ended with: %w (the worktree %s is kept for inspection)", err, work.Dir))
	}

	sha, err := work.Commit(ctx)
	if errors.Is(err, forge.ErrNoChange) {
		fmt.Fprintln(os.Stderr, "abhed: the agent changed nothing; no branch pushed, no pull request opened")
		return 2
	}
	if err != nil {
		// The run's change is only in the worktree until it is committed.
		kept = true
		return fail(fmt.Errorf("committing the change failed: %w (the worktree %s is kept for inspection)", err, work.Dir))
	}

	// Opening the request is a mutating action of its own, judged like one.
	pol := policy.New(policy.Mode(orDefault(cfg.Permissions.Mode, "default")))
	pol.Managed = cfg.Managed
	_ = pol.AddDeny(cfg.Permissions.Deny...)
	_ = pol.AddAsk(cfg.Permissions.Ask...)
	_ = pol.AddAllow(cfg.Permissions.Allow...)
	subject, _ := json.Marshal(map[string]string{"resource": ref.Owner + "/" + ref.Repo})
	verdict := pol.Evaluate("forge_pr", true, subject)
	switch {
	case verdict.Decision == policy.Deny:
		return fail(fmt.Errorf("opening a pull request on %s/%s is denied: %s", ref.Owner, ref.Repo, verdict.Reason))
	case verdict.Decision == policy.Allow && verdict.Step == "allow", *yes:
	case term.IsTerminal(int(os.Stdin.Fd())):
		fmt.Fprintf(os.Stderr, "Push %s (%s) and open a pull request against %s on %s? [y/N] ", work.Branch, sha, target, ref.Host)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
			fmt.Fprintf(os.Stderr, "abhed: not pushed; the change is on branch %s\n", work.Branch)
			return 4
		}
	default:
		return fail(fmt.Errorf("opening a pull request needs approval: pass -y, or add the rule forge_pr(%s/%s) to permissions.allow", ref.Owner, ref.Repo))
	}

	// The branch goes to the issue's own repository, from the operator's
	// checkout: a remote the agent could have changed is not consulted.
	if *remote != "" {
		fmt.Fprintf(os.Stderr, "abhed: -remote is ignored; pushing to %s\n", ref.Host)
	}
	if err := pushWork(ctx, work, workspace, ref, fg.PushAuth(), *ca); err != nil {
		return fail(err)
	}
	body := fmt.Sprintf("Resolves %s.\n\n```\n%s\n```\n", is.URL, work.Diff(ctx, workspace))
	url, err := fg.OpenPullRequest(ctx, ref, forge.PullRequest{Head: work.Branch, Base: target,
		Title: fmt.Sprintf("Resolve #%d: %s", ref.Number, is.Title), Body: body})
	if err != nil {
		return fail(fmt.Errorf("pushed %s, but opening the pull request failed: %w", work.Branch, err))
	}
	fmt.Println(url)
	return 0
}

// tokenFor reads the forge token from the environment, then the secrets store.
func tokenFor(kind string) string {
	name := strings.ToUpper(kind) + "_TOKEN"
	if v := os.Getenv(name); v != "" {
		return v
	}
	env, err := openVault().Env([]string{name})
	if err != nil || len(env) == 0 {
		return ""
	}
	return strings.TrimPrefix(env[0], name+"=")
}
