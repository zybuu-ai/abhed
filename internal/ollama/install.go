package ollama

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// Install kinds.
const (
	// InstallRun runs a package manager as the person, without sudo.
	InstallRun = "run"
	// InstallOpen opens the official download page; the person installs.
	InstallOpen = "open"
	// InstallScript runs the official install script, which may ask for the
	// person's password through sudo itself; only at a terminal.
	InstallScript = "script"
	// InstallManual is steps only: a package that needs sudo, or a system
	// Abhed does not install on.
	InstallManual = "manual"
)

// Official sources.
const (
	ScriptCommand  = "curl -fsSL https://ollama.com/install.sh | sh"
	MacDownload    = "https://ollama.com/download/mac"
	MacDMG         = "https://ollama.com/download/Ollama.dmg"
	WindowsPage    = "https://ollama.com/download/windows"
	LinuxPage      = "https://ollama.com/download/linux"
	HomebrewSource = "the Homebrew formula ollama (https://formulae.brew.sh/formula/ollama)"
)

// InstallPlan is how Ollama would be installed here, shown in full before
// anything runs.
type InstallPlan struct {
	Kind string `json:"kind"`
	// Display is the exact command, or the page opened.
	Display string `json:"command"`
	// Source says where the software comes from.
	Source string `json:"source"`
	// SizeURL is the artifact whose size is shown; "" when the installer
	// itself reports it.
	SizeURL string `json:"-"`
	// SizeNote says how big the download is, or why that is not known yet.
	SizeNote string `json:"size,omitempty"`
	// Manual are the person's own steps, for a manual plan or as the
	// fallback of any other.
	Manual []string `json:"steps,omitempty"`
	// NeedsTerminal is set when the step can only run at the person's own
	// terminal, where sudo can ask for a password.
	NeedsTerminal bool `json:"needs_terminal,omitempty"`
	path          string
	args          []string
}

// PlanInstall chooses how to install Ollama here. It never chooses a step
// that needs sudo: such a step is shown for the person to run.
func (e Env) PlanInstall() InstallPlan {
	brew, brewErr := e.look("brew")
	switch e.GOOS {
	case "darwin":
		if brewErr == nil {
			return InstallPlan{Kind: InstallRun, Display: "brew install ollama", Source: HomebrewSource,
				SizeNote: "Homebrew shows the size before it downloads", path: brew, args: []string{"install", "ollama"},
				Manual: []string{"brew install ollama"}}
		}
		open, _ := e.look("open")
		return InstallPlan{Kind: InstallOpen, Display: "open " + MacDownload, Source: "the official, signed Ollama app from ollama.com",
			SizeURL: MacDMG, path: open, args: []string{MacDownload},
			Manual: []string{"Download Ollama from " + MacDownload + ", open the .dmg and move Ollama to Applications", "Open Ollama once from Applications"}}
	case "linux":
		if brewErr == nil {
			return InstallPlan{Kind: InstallRun, Display: "brew install ollama", Source: HomebrewSource,
				SizeNote: "Homebrew shows the size before it downloads", path: brew, args: []string{"install", "ollama"},
				Manual: []string{"brew install ollama"}}
		}
		if pkg := e.distroPackage(); pkg != "" {
			return InstallPlan{Kind: InstallManual, Display: pkg, Source: "your distribution's ollama package",
				SizeNote: "the package manager shows the size", Manual: []string{pkg + "   (needs sudo, so run it yourself)"}}
		}
		sh, _ := e.look("sh")
		arch := runtime.GOARCH
		if arch != "arm64" {
			arch = "amd64"
		}
		return InstallPlan{Kind: InstallScript, Display: ScriptCommand, Source: "the official install script from ollama.com (see " + LinuxPage + ")",
			SizeURL: "https://ollama.com/download/ollama-linux-" + arch + ".tar.zst", NeedsTerminal: true,
			path: sh, args: []string{"-c", ScriptCommand},
			Manual: []string{ScriptCommand + "   (the script asks for sudo itself where it needs it)"}}
	case "windows":
		return InstallPlan{Kind: InstallManual, Display: WindowsPage, Source: "the official Ollama installer from ollama.com",
			SizeNote: "shown by the download", Manual: []string{"Download and run the installer from " + WindowsPage}}
	}
	return InstallPlan{Kind: InstallManual, Display: "https://ollama.com/download", Source: "ollama.com",
		Manual: []string{"See https://ollama.com/download for this system"}}
}

// distroPackage is the install command of a distribution that packages
// Ollama itself, from os-release; "" when none is known.
func (e Env) distroPackage() string {
	b, err := os.ReadFile(e.OSRelease)
	if err != nil {
		return ""
	}
	ids := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok || (k != "ID" && k != "ID_LIKE") {
			continue
		}
		for _, id := range strings.Fields(strings.Trim(v, `"'`)) {
			ids[id] = true
		}
	}
	if ids["arch"] {
		return "sudo pacman -S ollama"
	}
	return ""
}

// InstallSize fills in the plan's size from its artifact: a HEAD request,
// following the official redirects. A size that cannot be read is said to be.
func (e Env) InstallSize(ctx context.Context, p *InstallPlan) {
	if p.SizeURL == "" || p.SizeNote != "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, p.SizeURL, nil)
	if err == nil {
		var resp *http.Response
		if resp, err = e.web().Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 {
				p.SizeNote = "about " + HumanBytes(resp.ContentLength) + " (" + p.SizeURL + ", as it is now)"
				return
			}
		}
	}
	p.SizeNote = "not known until the download starts"
}

// Install runs the plan as the person, never with sudo. For an open plan
// it only opens the page, and the outcome is the person's to finish. A
// script plan runs only when terminal is set: it may ask for a password.
func (e Env) Install(ctx context.Context, p InstallPlan, terminal bool, stdin io.Reader, stdout, stderr io.Writer) *Problem {
	fail := func(kind, msg string) *Problem {
		return &Problem{Kind: kind, Message: msg, Steps: append(append([]string{}, p.Manual...), "Then choose the model again in Abhed."), Retry: true}
	}
	switch p.Kind {
	case InstallManual:
		return fail(KindManual, "Abhed does not install Ollama on this system; the steps below do.")
	case InstallScript:
		if !terminal {
			return fail(KindNeedsTerminal, "The official install script may ask for your password, so it runs only from Abhed in a terminal, or by you.")
		}
	}
	if p.path == "" {
		return fail(KindInstallFailed, "The program the install needs was not found.")
	}
	env := e.environ()
	if err := e.Run(ctx, p.path, p.args, env, stdin, stdout, stderr); err != nil {
		if ctx.Err() != nil {
			return fail(KindCancelled, "The install was stopped.")
		}
		return fail(KindInstallFailed, fmt.Sprintf("%s failed: %v", p.Display, err))
	}
	return nil
}
