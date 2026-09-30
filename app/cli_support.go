package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"

	root "github.com/zybuu-ai/abhed"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/doctor", Help: "check the configuration, the sandbox, the endpoint and tool calling", Group: "app", Order: 185, ReadOnly: true, Run: slashDoctor})
	registerSlash(slashCmd{Name: "/release-notes", Args: "[version|all]", Help: "what changed, from the changelog built into this binary", Group: "app", Order: 186, ReadOnly: true, Run: slashReleaseNotes})
	registerSlash(slashCmd{Name: "/bug", Args: "[what happened]", Help: "a prefilled issue link to open yourself; nothing is sent", Group: "app", Order: 187, ReadOnly: true, Run: slashBug})
}

// slashDoctor is /doctor inside a session: the checks `abhed doctor` makes
// that apply to a running session, without leaving it.
func slashDoctor(ctx context.Context, e *cmdEnv, _ []string) (bool, error) {
	st := e.st
	var b strings.Builder
	f := configFindings(&b, st.appCfg)
	rows := [][]string{{"check", "result"}}
	cfgResult := "ok"
	switch {
	case f.unknown && f.notInEffect:
		cfgResult = "keys nothing reads, and keys not yet in effect"
	case f.unknown:
		cfgResult = "keys nothing reads"
	case f.notInEffect:
		cfgResult = "keys this version does not act on yet"
	}
	rows = append(rows, []string{"configuration", cfgResult}, []string{"trust", trustLine(st.appCfg.Workspace)})
	if st.sandbox != nil {
		if sb, err := st.sandbox.wait(); err != nil {
			rows = append(rows, []string{"sandbox", "UNAVAILABLE: " + err.Error()})
		} else {
			rows = append(rows, []string{"sandbox", string(sb.Tier()) + " — " + sb.Describe()})
		}
	}
	p := st.provider
	probe := newEndpointProbe(p)
	if err := probe.run(ctx); err != nil {
		rows = append(rows, []string{"endpoint", friendlyModelError(err, st.appCfg.Model.Default, p)})
	} else {
		switch called, err := probeToolCalling(ctx, p); {
		case err != nil:
			rows = append(rows, []string{"endpoint", "answers; tool calling: " + friendlyModelError(err, st.appCfg.Model.Default, p)})
		case called:
			rows = append(rows, []string{"endpoint", "answers, and the model calls tools"})
		default:
			rows = append(rows, []string{"endpoint", "answers, but the model did not call the tool it was given"})
		}
	}
	if st.set != nil && st.set.Gateway != nil {
		for _, s := range st.set.Gateway.Servers() {
			if !s.Connected {
				rows = append(rows, []string{"mcp " + s.Name, "not connected; /mcp restart " + s.Name})
			}
		}
	}
	body := []ui.Block{{Kind: ui.BlockTable, Rows: rows}}
	if b.Len() > 0 {
		body = append(body, ui.Block{Kind: ui.BlockNotice, Text: strings.TrimSpace(b.String())})
	}
	return false, e.ui.Panel(ctx, ui.PanelSpec{Title: "Doctor", Body: body})
}

// releaseNotes is the changelog section for version: "" or "dev" is the
// unreleased section, "all" the latest three.
func releaseNotes(changelog, version string) (string, bool) {
	sections := strings.Split(changelog, "\n## [")
	if len(sections) < 2 {
		return "", false
	}
	sections = sections[1:]
	want := strings.TrimPrefix(version, "v")
	if want == "" || want == "dev" {
		want = "Unreleased"
	}
	if want == "all" {
		n := min(3, len(sections))
		return "## [" + strings.Join(sections[:n], "\n## ["), true
	}
	for _, s := range sections {
		if strings.HasPrefix(s, want+"]") {
			return "## [" + strings.TrimRight(s, "\n"), true
		}
	}
	return "", false
}

// slashReleaseNotes is /release-notes.
func slashReleaseNotes(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	v := e.st.version
	if len(args) > 0 {
		v = args[0]
	}
	notes, ok := releaseNotes(root.Changelog, v)
	if !ok {
		return false, fmt.Errorf("the changelog built in has no section for %s; /release-notes all shows the latest", v)
	}
	return false, e.ui.Panel(ctx, ui.PanelSpec{Title: "Release notes", Body: []ui.Block{{Kind: ui.BlockMarkdown, Text: notes}}})
}

// issueURL is where /bug points.
const issueURL = "https://github.com/zybuu-ai/abhed/issues/new"

// bugReport is the issue body: what the person wrote and the facts about
// this build and session that help, with secrets and home paths redacted.
// It names the provider's type, never its endpoint.
func bugReport(st *cliState, what string) (title, body string) {
	var b strings.Builder
	fmt.Fprintf(&b, "**What happened**\n\n%s\n\n**Environment**\n\n", orDefault(what, "(describe what you did and what you expected)"))
	fmt.Fprintf(&b, "- abhed %s on %s/%s\n", orDefault(st.version, "dev"), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "- terminal: %s\n", orDefault(os.Getenv("TERM"), "unknown"))
	fmt.Fprintf(&b, "- provider type: %s\n", orDefault(st.provider.Type, "unknown"))
	m := st.statusModel("")
	fmt.Fprintf(&b, "- sandbox: %s; record: %s\n", orDefault(m.SandboxTier, "unknown"), m.Record)
	text := b.String()
	vault := openVault().Redactor()
	keys := []string{st.provider.APIKey}
	if st.provider.APIKeyEnv != "" {
		keys = append(keys, os.Getenv(st.provider.APIKeyEnv))
	}
	redact := func(s string) string {
		// The redactor matches inside JSON strings, so the text goes in as one.
		enc, _ := json.Marshal(s)
		var out string
		if json.Unmarshal(vault.Redact(enc), &out) != nil {
			return "(withheld: the secrets store could not be read)"
		}
		s = out
		for _, k := range keys {
			if len(k) >= 4 {
				s = strings.ReplaceAll(s, k, "[redacted key]")
			}
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
			s = strings.ReplaceAll(s, home, "~")
		}
		return s
	}
	// Redacted before it is cut, so a cut cannot split what redaction matches.
	return oneLine(redact(orDefault(what, "Bug report")), 80), redact(text)
}

// slashBug is /bug: a link with the report filled in, for the person to
// open. Nothing leaves the machine unless they open it and submit it.
func slashBug(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	title, body := bugReport(e.st, strings.Join(args, " "))
	link := issueURL + "?" + url.Values{"title": {title}, "body": {body}}.Encode()
	e.ui.Append(ui.Block{Kind: ui.BlockMarkdown, Text: body})
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "nothing has been sent. To file it, open:"})
	e.ui.Append(ui.Block{Kind: ui.BlockToolOut, Text: link})
	// No answer, or no, leaves the link on the screen and opens nothing.
	if ans, _ := e.ui.Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm, Title: "Open this link in your browser?"}); ans != ui.ChoiceYes {
		return false, nil
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	if err := exec.CommandContext(ctx, opener, link).Start(); err != nil { // #nosec G204 -- the system opener, on a link the person just agreed to open
		return false, fmt.Errorf("could not open the browser: %w; the link is above", err)
	}
	return false, nil
}
