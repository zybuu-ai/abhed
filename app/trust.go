package app

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/frontmatter"
	"github.com/zybuu-ai/abhed/internal/ui"
	"golang.org/x/term"
)

// loadSession loads the configuration for a CLI session. On a terminal it
// asks once about an untrusted workspace file; anywhere else it only warns.
func loadSession(workspace string, trust config.TrustChoice, interactive bool) (config.Config, error) {
	ask := interactive && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust, Quiet: ask})
	if err != nil || !ask {
		return cfg, err
	}
	st := cfg.Workspace
	if !st.NeedsDecision() {
		warnTrust(st)
		return cfg, nil
	}
	grant, err := askTrust(os.Stdin, os.Stderr, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		warnTrust(st)
		return cfg, nil
	}
	if !grant {
		// Declining keeps whichever part was already trusted for this content:
		// new definitions do not cost a trusted file, nor a changed file
		// trusted definitions.
		if err := config.RecordDecision(workspace, st.Reviewed(), st.Trusted && st.Reason == "stored",
			st.AgentsTrusted && st.AgentsReason == "stored"); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: could not record the decision: %v\n", err)
		}
		if !st.Trusted {
			st.Reason = "declined"
		}
		if len(st.Agents) > 0 && !st.AgentsTrusted {
			st.AgentsReason = "declined"
		}
		cfg.Workspace = st
		warnTrust(st)
		return cfg, nil
	}
	if err := config.GrantReviewed(workspace, st.Reviewed()); err != nil {
		// The session goes on untrusted rather than ending on a failed write.
		fmt.Fprintf(os.Stderr, "abhed: could not record trust, so the file stays untrusted: %v\n", err)
		warnTrust(st)
		return cfg, nil
	}
	// Reloaded: the grant covers the content reviewed, and a file changed
	// since is untrusted again.
	return config.LoadWith(workspace, config.LoadOptions{Trust: trust})
}

func warnTrust(st config.WorkspaceTrust) {
	if s := st.Warning(); s != "" {
		fmt.Fprintf(os.Stderr, "abhed: warning: %s\n", s)
	}
}

// errNoAnswer is a prompt that ended without a decision.
var errNoAnswer = errors.New("no answer about the workspace configuration; it stays untrusted")

// askTrust shows what an untrusted file would change and asks whether to
// trust it. Only its number, 2, trusts it.
func askTrust(in io.Reader, out io.Writer, st config.WorkspaceTrust) (bool, error) {
	fmt.Fprintln(out, "\nThis workspace has its own Abhed configuration:")
	if st.File != "" {
		fmt.Fprintf(out, "  %s\n", config.Printable(st.File))
	}
	if len(st.Agents) > 0 {
		fmt.Fprintf(out, "  %s (%d agent definition(s))\n", config.Printable(filepath.Join(st.Workspace, filepath.FromSlash(config.WorkspaceAgentsDir))), len(st.Agents))
	}
	if st.Reason == "changed" || st.AgentsReason == "changed" {
		fmt.Fprintln(out, "It has changed since you trusted it.")
	} else {
		fmt.Fprintln(out, "You have not trusted it yet. A file that came with a repository can widen what the agent may do.")
		fmt.Fprintln(out, "Abhed now asks about workspace configuration, including files you wrote.")
	}
	describeTrust(out, st)
	// Ask about what is not yet trusted, never about what already is.
	question := "Trust this file?"
	agentsPending := len(st.Agents) > 0 && !st.AgentsTrusted
	switch {
	case agentsPending && st.File != "" && !st.Trusted:
		question = "Trust this file and these definitions?"
	case agentsPending:
		question = "Trust these definitions?"
	}
	// Numbered answers only, checked as every dialog is: nothing is chosen
	// for an empty line, and no letter trusts the file.
	spec, err := ui.DialogSpec{Kind: ui.DialogChoice, Title: question, Choices: []ui.Choice{
		{ID: ui.ChoiceNo, Label: "No, don't trust it"},
		{ID: "trust", Label: "Yes, trust it", Widening: true},
		{ID: "view", Label: "View the file"},
	}}.Normalized()
	if err != nil {
		return false, err
	}
	for {
		fmt.Fprintf(out, "\n%s\n", question)
		for i, c := range spec.Choices {
			fmt.Fprintf(out, "  %d. %s\n", i+1, c.Label)
		}
		fmt.Fprintf(out, "answer 1-%d: ", len(spec.Choices))
		line, err := readAnswer(in)
		if err != nil {
			fmt.Fprintln(out)
			return false, errNoAnswer
		}
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 1 || n > len(spec.Choices) {
			continue
		}
		switch spec.Choices[n-1].ID {
		case "trust":
			fmt.Fprintln(out, "Trusted. A later change to the file will be asked about again.")
			return true, nil
		case ui.ChoiceNo:
			fmt.Fprintln(out, "Not trusted. Only its tightening settings apply; `abhed trust grant` changes that.")
			return false, nil
		case "view":
			showFile(out, st)
		}
	}
}

// describeTrust lists what trusting the file would let it set, and what
// applies without trust.
func describeTrust(out io.Writer, st config.WorkspaceTrust) {
	if len(st.Ignored) > 0 {
		fmt.Fprintln(out, "Trusting it would let it set:")
		width := 0
		for _, k := range st.Ignored {
			width = max(width, len(k.Key))
		}
		for _, k := range st.Ignored {
			fmt.Fprintf(out, "  %-*s  %s%s\n", width, k.Key, k.Value, refusedNote(k))
		}
	}
	if len(st.Applied) > 0 {
		fmt.Fprintf(out, "Applied already, since they only tighten: %s\n", strings.Join(st.Applied, ", "))
	}
	if !st.AgentsTrusted {
		describeAgents(out, st, "Trusting them would let these agent definitions load:")
	}
}

// describeAgents lists the workspace's definitions: the name the model would
// call, the model each chooses and the tools each is limited to, read from
// the content the decision covers.
func describeAgents(out io.Writer, st config.WorkspaceTrust, heading string) {
	files := st.AgentFiles()
	if len(files) == 0 && len(st.AgentsProblems) == 0 {
		return
	}
	fmt.Fprintln(out, heading)
	for _, f := range files {
		name, model, tools := agentSummary(f)
		fmt.Fprintf(out, "  %s  model %s  tools %s  (%s)\n", name, model, tools, config.Printable(f.Rel))
	}
	for _, p := range st.AgentsProblems {
		fmt.Fprintf(out, "  never loaded: %s\n", p)
	}
}

// agentSummary is a definition's name, model and tools for display, with
// what an omitted key means.
func agentSummary(f config.AgentFile) (name, model, tools string) {
	name = strings.TrimSuffix(filepath.Base(f.Rel), ".md")
	model, tools = "inherit", "the session's"
	doc, err := frontmatter.Parse(string(f.Data))
	if err != nil {
		return config.Printable(name), model, "(no frontmatter: it will not load)"
	}
	for _, fl := range doc.Top() {
		switch strings.ToLower(fl.Key) {
		case "name":
			if fl.Value != "" {
				name = fl.Value
			}
		case "model":
			if fl.Value != "" {
				model = fl.Value
			}
		case "tools":
			if fl.Kind == frontmatter.List {
				tools = strings.Join(fl.List, ", ")
			} else if fl.Value != "" {
				tools = fl.Value
			}
		}
	}
	return config.Printable(name), config.Printable(model), config.Printable(tools)
}

// showFile prints the file only when it still holds what was classified, and
// the definitions as they were hashed.
func showFile(out io.Writer, st config.WorkspaceTrust) {
	for _, f := range st.AgentFiles() {
		fmt.Fprintf(out, "\n--- %s\n%s\n---\n", config.Printable(f.Path), config.PrintableText(string(bytes.TrimRight(f.Data, "\n"))))
	}
	if st.File == "" {
		return
	}
	data, err := os.ReadFile(st.File)
	if err != nil {
		fmt.Fprintf(out, "cannot read it: %v\n", err)
		return
	}
	if config.HashOf(data) != st.SHA256 {
		fmt.Fprintln(out, "The file changed while you were being asked. Answer d, and run abhed again to review the new version.")
		return
	}
	fmt.Fprintf(out, "\n--- %s\n%s\n---\n", config.Printable(st.File), config.PrintableText(string(bytes.TrimRight(data, "\n"))))
}

// readAnswer reads one line a byte at a time, so nothing past it is taken
// from the terminal the session then reads.
func readAnswer(in io.Reader) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < 256 {
		n, err := in.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return string(b), nil
			}
			b = append(b, one[0])
			continue
		}
		if err != nil {
			if len(b) > 0 {
				return string(b), nil
			}
			return "", err
		}
	}
	return string(b), nil
}

// refusedNote says why a value was refused outright, such as a rule that does not parse.
func refusedNote(k config.IgnoredKey) string {
	if k.Reason == "" {
		return ""
	}
	return "  (refused: " + k.Reason + ")"
}

// trustCmd is `abhed trust [show|grant [-sha256 H]|revoke|list] [dir]`.
func trustCmd(workspace string, args []string, out io.Writer) int {
	verb := "show"
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}
	// -sha256 pins a grant to the content that was reviewed.
	var want, wantAgents string
	if verb == "grant" {
		fs := flag.NewFlagSet("abhed trust grant", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		sum := fs.String("sha256", "", "grant only if the file still has this hash")
		agentsSum := fs.String("agents-sha256", "", "grant only if the agent definitions still have this hash")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		want, wantAgents, args = strings.ToLower(*sum), strings.ToLower(*agentsSum), fs.Args()
	}
	dir := workspace
	if len(args) > 0 {
		abs, err := filepath.Abs(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: trust: %v\n", err)
			return 2
		}
		dir = abs
	}
	switch verb {
	case "show":
		st, err := config.InspectWorkspace(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: trust: %v\n", err)
			return 1
		}
		printTrust(out, st)
		return 0
	case "grant":
		st, err := config.InspectWorkspace(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: trust: %v\n", err)
			return 1
		}
		if st.File == "" && len(st.Agents) == 0 {
			fmt.Fprintf(os.Stderr, "abhed: trust: %s has no .abhed/config.json or .abhed/agents to trust\n", st.Workspace)
			return 1
		}
		if st.Reason == "home" || st.AgentsReason == "home" {
			fmt.Fprintf(out, "%s is your own configuration and is always trusted.\n", config.Printable(filepath.Join(st.Workspace, ".abhed")))
			return 0
		}
		if want != "" && want != st.SHA256 {
			fmt.Fprintf(os.Stderr, "abhed: trust: %s has changed: its sha256 is %s, not the %s you reviewed. "+
				"Nothing was trusted; review it again with `abhed trust`\n", config.Printable(st.File), st.SHA256, config.Printable(want))
			return 1
		}
		if wantAgents != "" && wantAgents != st.AgentsSHA256 {
			fmt.Fprintf(os.Stderr, "abhed: trust: the agent definitions have changed: their sha256 is %s, not the %s you reviewed. "+
				"Nothing was trusted; review them again with `abhed trust`\n", st.AgentsSHA256, config.Printable(wantAgents))
			return 1
		}
		// The hashes are of the content just classified, not of a second read.
		if err := config.GrantReviewed(dir, st.Reviewed()); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: trust: %v\n", err)
			return 1
		}
		if st.File != "" {
			fmt.Fprintf(out, "Trusted %s (sha256 %s).\n", config.Printable(st.File), st.SHA256[:12])
			describeTrust(out, config.WorkspaceTrust{Ignored: st.Ignored})
		}
		if len(st.Agents) > 0 {
			fmt.Fprintf(out, "Trusted %d agent definition(s) (sha256 %s).\n", len(st.Agents), st.AgentsSHA256[:12])
		}
		fmt.Fprintln(out, "A later change to the file or a definition makes it untrusted again.")
		return 0
	case "revoke":
		had, err := config.RevokeTrust(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: trust: %v\n", err)
			return 1
		}
		if had {
			fmt.Fprintf(out, "Forgot the decision about %s; its configuration is untrusted again.\n", dir)
		} else {
			fmt.Fprintf(out, "There was no decision about %s.\n", dir)
		}
		return 0
	case "list":
		recs, err := config.TrustRecords()
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: trust: %v\n", err)
			return 1
		}
		keys := make([]string, 0, len(recs))
		for k := range recs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			r := recs[k]
			fmt.Fprintf(out, "%-8s  %s  %s  %s\n", r.Decision, r.SHA256[:min(12, len(r.SHA256))], r.At.Format("2006-01-02"), config.Printable(k))
		}
		if len(keys) == 0 {
			fmt.Fprintln(out, "No decisions recorded.")
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "abhed: trust: unknown action %q; use show, grant, revoke or list\n", verb)
	return 2
}

// trustLabel is the one-line state doctor and show print.
func trustLabel(st config.WorkspaceTrust) string {
	switch {
	case st.File == "":
		return "no workspace configuration"
	case st.Reason == "home":
		return "your own configuration"
	case st.Trusted:
		return map[string]string{"stored": "trusted", "flag": "trusted for this run (-trust-workspace)",
			"env": "trusted for this run (" + config.TrustEnv + ")"}[st.Reason]
	case st.Reason == "changed":
		return "NOT TRUSTED: changed since it was trusted"
	case st.Reason == "declined":
		return "NOT TRUSTED: you chose not to trust it"
	}
	return "NOT TRUSTED"
}

func printTrust(out io.Writer, st config.WorkspaceTrust) {
	fmt.Fprintf(out, "workspace   %s\n", config.Printable(st.Workspace))
	printAgentsTrust(out, st)
	if st.File == "" {
		fmt.Fprintln(out, "config      none")
		return
	}
	fmt.Fprintf(out, "config      %s\n", config.Printable(st.File))
	fmt.Fprintf(out, "sha256      %s\n", st.SHA256)
	fmt.Fprintf(out, "trust       %s\n", trustLabel(st))
	if st.Reason == "home" {
		return
	}
	if st.Trusted {
		fmt.Fprintln(out, "\nIt applies whole. Without trust these settings would be ignored:")
	} else {
		fmt.Fprintln(out, "\nIgnored until you trust it (`abhed trust grant`):")
	}
	if len(st.Ignored) == 0 {
		fmt.Fprintln(out, "  nothing: every setting in it only tightens")
	}
	for _, k := range st.Ignored {
		fmt.Fprintf(out, "  %s  %s%s\n", k.Key, k.Value, refusedNote(k))
	}
	if len(st.Applied) > 0 {
		fmt.Fprintf(out, "Applied either way, since they only tighten: %s\n", strings.Join(st.Applied, ", "))
	}
}

// agentsLabel is the one-line state of the workspace's agent definitions.
func agentsLabel(st config.WorkspaceTrust) string {
	switch {
	case st.AgentsReason == "home":
		return "your own definitions"
	case st.AgentsTrusted:
		return map[string]string{"stored": "trusted", "flag": "trusted for this run (-trust-workspace)",
			"env": "trusted for this run (" + config.TrustEnv + ")"}[st.AgentsReason]
	case st.AgentsReason == "changed":
		return "NOT TRUSTED: changed since they were trusted"
	case st.AgentsReason == "declined":
		return "NOT TRUSTED: you chose not to trust them"
	}
	return "NOT TRUSTED"
}

// printAgentsTrust is abhed trust's section on the workspace's definitions.
func printAgentsTrust(out io.Writer, st config.WorkspaceTrust) {
	if len(st.Agents) == 0 && len(st.AgentsProblems) == 0 {
		return
	}
	if len(st.Agents) > 0 {
		fmt.Fprintf(out, "agents      %d definition(s) in %s\n", len(st.Agents), config.WorkspaceAgentsDir)
		fmt.Fprintf(out, "sha256      %s\n", st.AgentsSHA256)
		fmt.Fprintf(out, "trust       %s\n", agentsLabel(st))
	}
	heading := "Not loaded until you trust them (`abhed trust grant`):"
	if st.AgentsTrusted {
		heading = "Loaded:"
	}
	describeAgents(out, st, heading)
	fmt.Fprintln(out)
}

// printDoctorTrust is doctor's line on the workspace file, naming each
// setting an untrusted one had ignored.
func printDoctorTrust(out io.Writer, st config.WorkspaceTrust) {
	if len(st.Agents) > 0 {
		fmt.Fprintf(out, "agents      %s — %d definition(s) in %s\n", agentsLabel(st), len(st.Agents), config.WorkspaceAgentsDir)
		for _, a := range st.IgnoredAgents() {
			fmt.Fprintf(out, "            ⚠ ignored %s\n", a)
		}
	}
	for _, p := range st.AgentsProblems {
		fmt.Fprintf(out, "            ⚠ never loaded: %s\n", p)
	}
	if st.File == "" {
		return
	}
	fmt.Fprintf(out, "trust       %s — %s\n", trustLabel(st), config.Printable(st.File))
	for _, k := range st.Ignored {
		fmt.Fprintf(out, "            ⚠ ignored %s %s%s\n", k.Key, k.Value, refusedNote(k))
	}
	if len(st.Ignored) > 0 {
		fmt.Fprintln(out, "            review it with `abhed trust`; trust it with `abhed trust grant`")
	}
}

// ignoredModelKeys are the model settings an untrusted file could not make.
func ignoredModelKeys(st config.WorkspaceTrust) []string {
	var out []string
	for _, k := range st.Ignored {
		if k.Key == "model" || strings.HasPrefix(k.Key, "model.") || strings.HasPrefix(k.Key, "custom_providers") {
			out = append(out, k.Key)
		}
	}
	return out
}

// noteIgnoredModel repeats, where a run failed, that the model it used was
// not the one the untrusted workspace file names; CI logs bury the warning.
func noteIgnoredModel(cfg config.Config) {
	keys := ignoredModelKeys(cfg.Workspace)
	if len(keys) == 0 {
		return
	}
	endpoint := "its default endpoint"
	if p, err := cfg.Provider(); err == nil && p.BaseURL != "" {
		endpoint = p.BaseURL
	}
	fmt.Fprintf(os.Stderr, "abhed: note: the workspace configuration's model settings (%s) were ignored because it is not trusted; "+
		"this run used provider %q at %s. Trust it with `abhed trust grant`, or -trust-workspace for one run\n",
		strings.Join(keys, ", "), config.Printable(cfg.Model.Default), config.PrintableURL(endpoint))
}
