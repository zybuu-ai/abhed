package ollama

import (
	"fmt"
	"strings"
)

// Kinds of Problem.
const (
	KindNotInstalled  = "not_installed"
	KindInstallFailed = "install_failed"
	KindNoNetwork     = "no_network"
	KindPortBusy      = "port_busy"
	KindNotReady      = "not_ready"
	KindStartFailed   = "start_failed"
	KindDiskShort     = "disk_short"
	KindRAMShort      = "ram_short"
	KindPullFailed    = "pull_failed"
	KindModelNotFound = "model_not_found"
	KindNotPulled     = "not_pulled"
	KindOtherModels   = "other_models_folder"
	KindNotRunning    = "not_running"
	KindRemote        = "remote"
	KindAssistOff     = "assist_off"
	KindNotAdmin      = "not_admin"
	KindInAgent       = "in_agent"
	KindCancelled     = "cancelled"
	KindNotOllama     = "not_ollama"
	KindManual        = "manual_steps"
)

// Problem is why a local model is not working, or why Abhed would not act:
// one plain message, and the numbered steps the person can run themselves.
type Problem struct {
	Kind    string   `json:"kind"`
	Message string   `json:"message"`
	Steps   []string `json:"steps"`
	// Retry is set when trying again may work once the steps are done.
	Retry bool `json:"retry"`
}

func (p *Problem) Error() string { return p.Message }

// Text is the message and the numbered steps, for a terminal.
func (p *Problem) Text() string {
	var b strings.Builder
	b.WriteString(p.Message)
	if len(p.Steps) > 0 {
		b.WriteString("\nTo do it yourself:")
		for i, s := range p.Steps {
			fmt.Fprintf(&b, "\n  %d. %s", i+1, s)
		}
	}
	return b.String()
}

// Steps are the manual steps from the state st describes to a working
// model: install, start, pull, then choose the model again.
func (e Env) Steps(t Target, st Status) []string {
	var out []string
	if !st.Installed && !st.Running {
		out = append(out, e.installSteps()...)
	}
	if !st.Running {
		out = append(out, e.startStep(t, st))
	}
	if !st.Pulled && t.Model != "" {
		out = append(out, t.PullCommand())
	}
	out = append(out, retryStep(t))
	return out
}

func retryStep(t Target) string {
	if t.Provider == "" {
		return "Choose the model again in Abhed."
	}
	return fmt.Sprintf("Choose %s again in Abhed (in the CLI: /model %s).", t.Provider, t.Provider)
}

// installSteps are the person's own steps to install Ollama here.
func (e Env) installSteps() []string {
	p := e.PlanInstall()
	if len(p.Manual) > 0 {
		return p.Manual
	}
	return []string{p.Display}
}

// startStep is the person's own command to start the server.
// It leads with `ollama serve` bound to loopback, as Abhed itself starts it.
func (e Env) startStep(t Target, st Status) string {
	serve := "OLLAMA_HOST=" + t.serveHost() + " ollama serve"
	switch {
	case e.GOOS == "windows":
		return "set OLLAMA_HOST=" + t.serveHost() + " && ollama serve   (or start Ollama from the Start menu, which listens where its own settings say)"
	case e.GOOS == "darwin" && st.App != "" && t.Default():
		return serve + "   (or open the Ollama app, which listens where its own settings say)"
	}
	return serve
}

func (e Env) problem(kind, msg string, t Target, st Status) *Problem {
	return &Problem{Kind: kind, Message: msg, Steps: e.Steps(t, st), Retry: true}
}
