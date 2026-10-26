package toolchain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// ToolName is the toolchain tool's name.
const ToolName = "toolchain"

// Tool installs a language toolchain for the session's commands, or lists
// what is installed. The version is the one asked for, else the project's
// pin, else the latest stable; the approval names the source, the size and
// the published checksum, and the scope it offers is the language and its
// major version, so a person agrees once per language and major.
type Tool struct {
	S *Session
}

func (Tool) Name() string  { return ToolName }
func (Tool) Mutates() bool { return false }

// MutatesCall: listing changes nothing; an install does.
func (Tool) MutatesCall(raw json.RawMessage) bool {
	var a toolArgs
	_ = json.Unmarshal(raw, &a)
	return !a.List
}

func (Tool) Description() string {
	return "Install a language toolchain the session's commands need, or list what is installed: " + Names() +
		". Give name (\"go\", \"python\", \"node\"…) and, only when the user asked for one, version (\"1.22\", \"3.11\"); " +
		"without it the version the project pins is used, else the latest stable. The user is asked before anything is installed. " +
		"Use it when a command fails because go, python3, node, cargo, java, mvn, gradle, ruby or dotnet is not found, then run the command again."
}

func (Tool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "name":{"type":"string","description":"The language: go, python, node, rust, java, maven, gradle, ruby or dotnet."},
    "version":{"type":"string","description":"Only when the user asked for one: a version or prefix such as 1.22 or 3.11."},
    "list":{"type":"boolean","description":"List the installed toolchains and the project's pins instead of installing."},
    "resolved":{"type":"object","description":"Filled in by Abhed: what will be installed. Do not set it."}
  }
}`)
}

type toolArgs struct {
	Name     string    `json:"name"`
	Version  string    `json:"version"`
	List     bool      `json:"list"`
	Resolved *resolved `json:"resolved,omitempty"`
}

// resolved is what an approval shows and an install must match.
type resolved struct {
	Version string `json:"version"`
	Why     string `json:"why"`
	Source  string `json:"source,omitempty"`
	URL     string `json:"url,omitempty"`
	Digest  string `json:"digest,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Cached  bool   `json:"cached,omitempty"`
}

// planTimeout bounds asking a release source what to install.
const planTimeout = time.Minute

func (t Tool) request(a toolArgs) (Spec, error) {
	name, ver := a.Name, a.Version
	if n, v, ok := strings.Cut(name, "@"); ok {
		name = n
		if ver == "" {
			ver = v
		}
	}
	spec, err := ParseSpec(name)
	if err != nil {
		return Spec{}, err
	}
	if spec.Version, err = NormalizeVersion(spec.Lang, ver); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// ResolveArgs puts the call in the form an approval judges: name becomes
// language@major, and resolved what will be installed, whatever the model
// wrote there.
func (t Tool) ResolveArgs(ts *tools.Session, raw json.RawMessage) (json.RawMessage, []string, error) {
	var a toolArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, nil, err
	}
	if a.List {
		return json.RawMessage(`{"list":true}`), nil, nil
	}
	spec, err := t.request(a)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), planTimeout)
	defer cancel()
	plan, err := t.at(ts, nil).Plan(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	r := &resolved{Version: plan.Version(), Why: plan.Why, Source: plan.Artifact.Source, URL: plan.Artifact.URL,
		Digest: plan.Artifact.Digest, Size: plan.Artifact.Size, Cached: plan.Installed != nil}
	out, err := json.Marshal(toolArgs{Name: plan.Spec.Lang + "@" + Major(plan.Spec.Lang, plan.Version()), Version: plan.Spec.Version, Resolved: r})
	return out, []string{"name", "resolved"}, err
}

// Target names what the install fetches, for the approval and the record.
func (t Tool) Target(_ *tools.Session, raw json.RawMessage) string {
	var a toolArgs
	if json.Unmarshal(raw, &a) != nil || a.List || a.Resolved == nil {
		return ""
	}
	r := a.Resolved
	l, _ := Find(strings.SplitN(a.Name, "@", 2)[0])
	opens := ""
	if t.S.Registries != nil {
		if hosts, _ := t.S.Registries(l.Name); len(hosts) > 0 {
			opens = "; and opens its package registries to commands: " + strings.Join(hosts, ", ")
		}
	}
	if r.Cached {
		return fmt.Sprintf("%s %s, installed already%s", l.Display, r.Version, opens)
	}
	size := "size not published"
	if r.Size > 0 {
		size = fmt.Sprintf("%.1f MB", float64(r.Size)/1e6)
	}
	return fmt.Sprintf("%s %s (%s) from %s: %s · %s · %s%s", l.Display, r.Version, r.Why, r.Source, r.URL, size, r.Digest, opens)
}

// at is the session for a call: its workspace, and its events gathered
// into the result.
func (t Tool) at(ts *tools.Session, events *[]tools.Event) *Session {
	s := *t.S
	if ts != nil && ts.Root != "" {
		s.Workspace = ts.WorkDir()
	}
	if events != nil {
		s.Record = func(a agent.ToolchainAction) {
			*events = append(*events, tools.Event{Type: string(agent.EvToolchainInstall), Payload: a})
		}
	}
	return &s
}

func (t Tool) Run(ctx context.Context, ts *tools.Session, raw json.RawMessage) tools.Result {
	var a toolArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return tools.Result{Content: "Invalid arguments for toolchain: " + err.Error(), IsError: true}
	}
	if a.List {
		return tools.Result{Content: t.list(t.at(ts, nil))}
	}
	spec, err := t.request(a)
	if err != nil {
		return tools.Result{Content: err.Error(), IsError: true}
	}
	var events []tools.Event
	s := t.at(ts, &events)
	plan, err := s.Plan(ctx, spec)
	if err != nil {
		return tools.Result{Content: err.Error(), IsError: true}
	}
	// What runs is what was approved, or nothing.
	if r := a.Resolved; r == nil || r.Version != plan.Version() || plan.Installed == nil && r.Digest != plan.Artifact.Digest {
		return tools.Result{Content: "What would be installed has changed since it was approved, so nothing was installed; call toolchain again.", IsError: true}
	}
	in, err := s.Install(ctx, plan, "agent", "tool", "call", nil)
	if err != nil {
		return tools.Result{Content: "The install failed: " + err.Error(), IsError: true, Events: events}
	}
	l, _ := Find(in.Lang)
	how := "downloaded and verified against " + in.Digest
	if plan.Installed != nil {
		how = "already installed"
	}
	reach := s.openRegistries(l.Name, "agent", "tool", "call", &events)
	return tools.Result{Content: fmt.Sprintf("%s %s is installed (%s) and on PATH for the session's commands: %s.%s Run the command again.",
		l.Display, in.Version, how, strings.Join(BinDirs(in), ":"), reach), Events: events}
}

func (t Tool) list(s *Session) string {
	var b strings.Builder
	act := s.Active(s.Workspace)
	if len(act) == 0 {
		b.WriteString("No toolchains are installed for this session's commands.\n")
	}
	for _, in := range act {
		fmt.Fprintf(&b, "%s %s (on PATH)\n", in.Lang, in.Version)
	}
	for lang, p := range PinsFrom(s.Workspace) {
		fmt.Fprintf(&b, "%s pins %s %s\n", p.File, lang, p.Spec.Version)
	}
	return strings.TrimSpace(b.String())
}
