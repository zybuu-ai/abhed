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
	"strings"

	"github.com/zybuu-ai/abhed/config"
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
		if err := config.DeclineTrust(workspace, st.SHA256); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: could not record the decision: %v\n", err)
		}
		st.Reason = "declined"
		cfg.Workspace = st
		warnTrust(st)
		return cfg, nil
	}
	if err := config.GrantTrust(workspace, st.SHA256); err != nil {
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
// trust it. Only an explicit "t" trusts it.
func askTrust(in io.Reader, out io.Writer, st config.WorkspaceTrust) (bool, error) {
	fmt.Fprintf(out, "\nThis workspace has its own Abhed configuration:\n  %s\n", config.Printable(st.File))
	if st.Reason == "changed" {
		fmt.Fprintln(out, "It has changed since you trusted it.")
	} else {
		fmt.Fprintln(out, "You have not trusted it yet. A file that came with a repository can widen what the agent may do.")
		fmt.Fprintln(out, "Since 1.2.2, Abhed asks about workspace configuration, including files you wrote.")
	}
	describeTrust(out, st)
	for {
		fmt.Fprint(out, "\nTrust this file? [t]rust  [d]on't trust  [v]iew the file: ")
		line, err := readAnswer(in)
		if err != nil {
			fmt.Fprintln(out)
			return false, errNoAnswer
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "t", "trust":
			fmt.Fprintln(out, "Trusted. A later change to the file will be asked about again.")
			return true, nil
		case "d", "n", "no", "don't", "dont":
			fmt.Fprintln(out, "Not trusted. Only its tightening settings apply; `abhed trust grant` changes that.")
			return false, nil
		case "v", "view":
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
}

// showFile prints the file only when it still holds what was classified.
func showFile(out io.Writer, st config.WorkspaceTrust) {
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
	var want string
	if verb == "grant" {
		fs := flag.NewFlagSet("abhed trust grant", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		sum := fs.String("sha256", "", "grant only if the file still has this hash")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		want, args = strings.ToLower(*sum), fs.Args()
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
		if st.File == "" {
			fmt.Fprintf(os.Stderr, "abhed: trust: %s has no .abhed/config.json to trust\n", st.Workspace)
			return 1
		}
		if st.Reason == "home" {
			fmt.Fprintf(out, "%s is your own configuration and is always trusted.\n", config.Printable(st.File))
			return 0
		}
		if want != "" && want != st.SHA256 {
			fmt.Fprintf(os.Stderr, "abhed: trust: %s has changed: its sha256 is %s, not the %s you reviewed. "+
				"Nothing was trusted; review it again with `abhed trust`\n", config.Printable(st.File), st.SHA256, config.Printable(want))
			return 1
		}
		// The hash is of the content just classified, not of a second read.
		if err := config.GrantTrust(dir, st.SHA256); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: trust: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "Trusted %s (sha256 %s).\n", config.Printable(st.File), st.SHA256[:12])
		describeTrust(out, config.WorkspaceTrust{Ignored: st.Ignored})
		fmt.Fprintln(out, "A later change to the file makes it untrusted again.")
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

// printDoctorTrust is doctor's line on the workspace file, naming each
// setting an untrusted one had ignored.
func printDoctorTrust(out io.Writer, st config.WorkspaceTrust) {
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
		strings.Join(keys, ", "), config.Printable(cfg.Model.Default), config.Printable(endpoint))
}
