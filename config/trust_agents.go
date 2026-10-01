package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/internal/nlink"
)

// A workspace's .abhed/agents/*.md files define subagents. A definition is
// more than instructions: it can choose a model, and so a provider that
// receives the code, and it sets turn caps and tool lists. So the files load
// only under a trust decision bound to their exact content.

// WorkspaceAgentsDir is where a workspace keeps its agent definitions.
const WorkspaceAgentsDir = ".abhed/agents"

// Bounds on what is read from a workspace's agents directory: a definition is
// a short file, and a repository should not make startup read without limit.
const (
	maxAgentFiles     = 64
	maxAgentFileBytes = 64 << 10
)

// AgentFile is one definition file read from the workspace, as hashed.
type AgentFile struct {
	// Rel is the path relative to the workspace, such as .abhed/agents/x.md.
	Rel string
	// Path is the absolute path it was read from.
	Path string
	// Data is the content the trust hash covers.
	Data []byte
}

// Reviewed is the content a trust decision covers: the configuration file's
// hash and the agent definitions' combined hash. Either may be empty.
type Reviewed struct {
	SHA256       string
	AgentsSHA256 string
}

// Reviewed is what this load classified, for a decision about it.
func (w WorkspaceTrust) Reviewed() Reviewed {
	return Reviewed{SHA256: w.SHA256, AgentsSHA256: w.AgentsSHA256}
}

// AgentFiles are the workspace's definition files as read and hashed. The
// loader takes them from here, not from a second read, so what loads is what
// the trust decision covered.
func (w WorkspaceTrust) AgentFiles() []AgentFile {
	return append([]AgentFile(nil), w.agentFiles...)
}

// IgnoredAgents names the workspace's definitions that were not loaded
// because they are not trusted, as agents/<name>.
func (w WorkspaceTrust) IgnoredAgents() []string {
	if w.AgentsTrusted {
		return nil
	}
	out := make([]string, 0, len(w.Agents))
	for _, rel := range w.Agents {
		out = append(out, "agents/"+strings.TrimSuffix(filepath.Base(rel), ".md"))
	}
	return out
}

// agentsNeedDecision is a workspace with definitions that are not trusted
// and have had no answer for this content.
func (w WorkspaceTrust) agentsNeedDecision() bool {
	return len(w.Agents) > 0 && !w.AgentsTrusted && (w.AgentsReason == "new" || w.AgentsReason == "changed")
}

// readWorkspaceAgents reads <workspace>/.abhed/agents/*.md into st and hashes
// them. A file that is a link, has a second name or is too large is refused
// and never loaded, so the hash need not cover it.
func readWorkspaceAgents(workspace string, st *WorkspaceTrust) {
	dir := filepath.Join(workspace, filepath.FromSlash(WorkspaceAgentsDir))
	for _, d := range []string{filepath.Join(workspace, ".abhed"), dir} {
		info, err := os.Lstat(d)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			st.AgentsProblems = append(st.AgentsProblems, fmt.Sprintf("%s: %v", Printable(d), err))
			return
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			st.AgentsProblems = append(st.AgentsProblems, fmt.Sprintf("%s is not a directory of its own; no definition is read from it", Printable(d)))
			return
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		st.AgentsProblems = append(st.AgentsProblems, fmt.Sprintf("%s: %v", Printable(dir), err))
		return
	}
	var files []AgentFile
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		if len(files) == maxAgentFiles {
			st.AgentsProblems = append(st.AgentsProblems, fmt.Sprintf("more than %d definitions in %s; the rest are not read", maxAgentFiles, Printable(dir)))
			break
		}
		path := filepath.Join(dir, name)
		data, err := ReadAgentFile(path)
		if err != nil {
			st.AgentsProblems = append(st.AgentsProblems, fmt.Sprintf("%s: %v", Printable(path), err))
			continue
		}
		files = append(files, AgentFile{Rel: WorkspaceAgentsDir + "/" + name, Path: path, Data: data})
	}
	if len(files) == 0 {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Rel < files[j].Rel })
	h := sha256.New()
	for _, f := range files {
		st.Agents = append(st.Agents, f.Rel)
		fmt.Fprintf(h, "%s\x00%s\n", f.Rel, hashOf(f.Data))
	}
	st.AgentsSHA256 = hex.EncodeToString(h.Sum(nil))
	st.agentFiles = files
}

// ReadAgentFile reads a definition that is a regular file with one name, and
// is still the file that was checked once it is open.
func ReadAgentFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file; a link is not followed")
	}
	return ReadAgentOpened(path, openAgent, func(opened os.FileInfo) error {
		if !os.SameFile(info, opened) {
			return fmt.Errorf("changed while it was read")
		}
		return nil
	})
}

// openAgent opens a definition; a variable so a test can swap the file
// between the check on the path and the open.
var openAgent = os.Open

// ReadAgentOpened opens path once, applies every rule to that open file (a
// regular file, one name, check) and reads from it, so what is checked is what
// is read, whatever happens to the path meanwhile.
func ReadAgentOpened(path string, open func(string) (*os.File, error), check func(os.FileInfo) error) ([]byte, error) {
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if err := check(opened); err != nil {
		return nil, err
	}
	if n := nlink.Of(opened); n > 1 {
		return nil, fmt.Errorf("has %d names; a definition with a second name is not read", n)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxAgentFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAgentFileBytes {
		return nil, fmt.Errorf("larger than %d KiB", maxAgentFileBytes>>10)
	}
	return data, nil
}

// decideAgents is decide for the definitions: a caller's choice, then
// TrustEnv, then the stored decision for exactly this content.
func decideAgents(st WorkspaceTrust, o LoadOptions) (bool, string) {
	if len(st.Agents) == 0 {
		return false, "none"
	}
	switch o.Trust {
	case TrustRefused:
		return false, "refused"
	case TrustGranted:
		return true, "flag"
	}
	if v := os.Getenv(TrustEnv); v == "1" || strings.EqualFold(v, "true") {
		return true, "env"
	}
	e, ok, err := lookupTrust(st.Workspace)
	if err != nil {
		return false, "new"
	}
	switch {
	case !ok || e.AgentsSHA256 == "":
		// A record from before definitions were covered decided nothing about them.
		return false, "new"
	case e.AgentsSHA256 != st.AgentsSHA256:
		return false, "changed"
	case orDecision(e.AgentsDecision, e.Decision) == decisionTrusted:
		return true, "stored"
	}
	return false, "declined"
}

func orDecision(d, fallback string) string {
	if d == "" {
		return fallback
	}
	return d
}

// isHome reports whether the workspace is the home directory, whose .abhed
// is the user's own.
func isHome(workspace string) bool {
	home, err := os.UserHomeDir()
	return err == nil && canonical(home) == canonical(workspace)
}

// RefreshAgents re-reads the workspace's definitions for a reload. Content
// the load already trusted stays trusted on the same terms; anything else is
// trusted only by a stored decision for exactly that content, or TrustEnv.
func RefreshAgents(loaded WorkspaceTrust) WorkspaceTrust {
	st := WorkspaceTrust{Workspace: loaded.Workspace}
	readWorkspaceAgents(loaded.Workspace, &st)
	switch {
	case len(st.Agents) == 0:
		st.AgentsReason = "none"
	case st.AgentsSHA256 == loaded.AgentsSHA256:
		st.AgentsTrusted, st.AgentsReason = loaded.AgentsTrusted, loaded.AgentsReason
	case isHome(loaded.Workspace):
		st.AgentsTrusted, st.AgentsReason = true, "home"
	case loaded.AgentsReason == "refused":
		st.AgentsReason = "refused"
	default:
		st.AgentsTrusted, st.AgentsReason = decideAgents(st, LoadOptions{})
	}
	return st
}
