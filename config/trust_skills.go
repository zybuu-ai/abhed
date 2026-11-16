package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/internal/skills"
)

// A workspace's .abhed/skills/<name>/ folders are skills: a SKILL.md and the
// files beside it. A skill's body is instructions to the agent, and a
// pipeline beside it runs tool steps, so a workspace's skills load only
// under a trust decision bound to the exact content of every file in them,
// as its agent definitions do. The managed configuration, or anyone's, can
// turn them off with skills.disable_workspace.

// WorkspaceSkillsDir is where a workspace keeps its skills.
const WorkspaceSkillsDir = ".abhed/skills"

// Bounds on what is read from a workspace's skills: a repository should not
// make startup read without limit.
const (
	maxWorkspaceSkills = 64
	maxSkillFiles      = 64
	maxSkillDepth      = 4
	maxSkillFileBytes  = 256 << 10
	maxSkillBytes      = 1 << 20
)

// SkillFile is one file of a workspace skill, as hashed.
type SkillFile struct {
	// Rel is the path under the skill's folder, slash-separated.
	Rel  string
	Data []byte
}

// WorkspaceSkill is one skill of the workspace, as the trust decision covers it.
type WorkspaceSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Dir is the skill's folder relative to the workspace, such as .abhed/skills/release.
	Dir string `json:"dir"`
	// SHA256 is one hash over every file of the skill, by path and content.
	SHA256 string `json:"sha256"`
	// Path is the folder's absolute path.
	Path  string      `json:"-"`
	Files []SkillFile `json:"-"`
}

// SkillFiles are the workspace's skills as read and hashed. The loader takes
// them from here, not from a second read, so what loads is what the trust
// decision covered.
func (w WorkspaceTrust) SkillFiles() []WorkspaceSkill {
	return append([]WorkspaceSkill(nil), w.skillFiles...)
}

// IgnoredSkills names the workspace's skills that were not loaded because
// they are not trusted, as skills/<name>.
func (w WorkspaceTrust) IgnoredSkills() []string {
	if w.SkillsTrusted || w.SkillsReason == "off" {
		return nil
	}
	out := make([]string, 0, len(w.Skills))
	for _, s := range w.Skills {
		out = append(out, "skills/"+s.Name)
	}
	return out
}

// skillsNeedDecision is a workspace with skills that are not trusted and
// have had no answer for this content.
func (w WorkspaceTrust) skillsNeedDecision() bool {
	return len(w.Skills) > 0 && !w.SkillsTrusted && (w.SkillsReason == "new" || w.SkillsReason == "changed")
}

// readWorkspaceSkills reads every skill folder under <workspace>/.abhed/skills
// into st and hashes it. A skill holding a link, a file with a second name, a
// file too large, too many files or a SKILL.md that does not parse is refused
// whole and never loaded, so the hash need not cover it.
func readWorkspaceSkills(workspace string, st *WorkspaceTrust) {
	dir := filepath.Join(workspace, filepath.FromSlash(WorkspaceSkillsDir))
	for _, d := range []string{filepath.Join(workspace, ".abhed"), dir} {
		info, err := os.Lstat(d)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			st.SkillsProblems = append(st.SkillsProblems, fmt.Sprintf("%s: %v", Printable(d), err))
			return
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			st.SkillsProblems = append(st.SkillsProblems, fmt.Sprintf("%s is not a directory of its own; no skill is read from it", Printable(d)))
			return
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		st.SkillsProblems = append(st.SkillsProblems, fmt.Sprintf("%s: %v", Printable(dir), err))
		return
	}
	var found []WorkspaceSkill
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if !e.IsDir() {
			if e.Type()&os.ModeSymlink != 0 {
				st.SkillsProblems = append(st.SkillsProblems, fmt.Sprintf("%s is a link; a workspace skill is a folder of its own", Printable(path)))
			}
			continue
		}
		if len(found) == maxWorkspaceSkills {
			st.SkillsProblems = append(st.SkillsProblems, fmt.Sprintf("more than %d skills in %s; the rest are not read", maxWorkspaceSkills, Printable(dir)))
			break
		}
		sk, err := readWorkspaceSkill(path, WorkspaceSkillsDir+"/"+e.Name())
		if err != nil {
			st.SkillsProblems = append(st.SkillsProblems, fmt.Sprintf("%s: %v", Printable(path), err))
			continue
		}
		if sk != nil {
			found = append(found, *sk)
		}
	}
	if len(found) == 0 {
		return
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Dir < found[j].Dir })
	h := sha256.New()
	for _, s := range found {
		fmt.Fprintf(h, "%s\x00%s\n", s.Dir, s.SHA256)
		info := s
		info.Files = nil
		st.Skills = append(st.Skills, info)
	}
	st.SkillsSHA256 = hex.EncodeToString(h.Sum(nil))
	st.skillFiles = found
}

// readWorkspaceSkill reads one skill folder; nil when it has no SKILL.md.
func readWorkspaceSkill(path, rel string) (*WorkspaceSkill, error) {
	if _, err := os.Lstat(filepath.Join(path, "SKILL.md")); os.IsNotExist(err) {
		return nil, nil
	}
	var files []SkillFile
	total := 0
	var walk func(dir, under string, depth int) error
	walk = func(dir, under string, depth int) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			p, r := filepath.Join(dir, e.Name()), under+e.Name()
			switch {
			case e.IsDir():
				if depth == maxSkillDepth {
					return fmt.Errorf("%s is nested deeper than %d folders", r, maxSkillDepth)
				}
				if err := walk(p, r+"/", depth+1); err != nil {
					return err
				}
				continue
			case !e.Type().IsRegular():
				return fmt.Errorf("%s is not a regular file; a skill with a link is not loaded", r)
			}
			if len(files) == maxSkillFiles {
				return fmt.Errorf("holds more than %d files", maxSkillFiles)
			}
			data, err := readFileWithin(p, maxSkillFileBytes)
			if err != nil {
				return fmt.Errorf("%s: %w", r, err)
			}
			if total += len(data); total > maxSkillBytes {
				return fmt.Errorf("holds more than %d KiB", maxSkillBytes>>10)
			}
			files = append(files, SkillFile{Rel: r, Data: data})
		}
		return nil
	}
	if err := walk(path, "", 0); err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Rel < files[j].Rel })
	var body []byte
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%s\n", f.Rel, hashOf(f.Data))
		if f.Rel == "SKILL.md" {
			body = f.Data
		}
	}
	parsed, err := skills.Parse(string(body))
	if err != nil {
		return nil, fmt.Errorf("SKILL.md: %w", err)
	}
	name := parsed.Name
	if name == "" {
		name = filepath.Base(path)
	}
	return &WorkspaceSkill{Name: name, Description: parsed.Description, Dir: rel, Path: path,
		SHA256: hex.EncodeToString(h.Sum(nil)), Files: files}, nil
}

// decideSkills is decide for the skills: a caller's choice, then TrustEnv,
// then the stored decision for exactly this content.
func decideSkills(st WorkspaceTrust, o LoadOptions) (bool, string) {
	if len(st.Skills) == 0 {
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
	case !ok || e.SkillsSHA256 == "":
		// A record from before skills were covered decided nothing about them.
		return false, "new"
	case e.SkillsSHA256 != st.SkillsSHA256:
		return false, "changed"
	case orDecision(e.SkillsDecision, e.Decision) == decisionTrusted:
		return true, "stored"
	}
	return false, "declined"
}

// skillsOff turns the workspace's skills off when the configuration says
// so: skills.disabled, or skills.disable_workspace, which any layer may set.
func skillsOff(cfg *Config) {
	if len(cfg.Workspace.Skills) > 0 && (cfg.Skills.Disabled || cfg.Skills.DisableWorkspace) {
		cfg.Workspace.SkillsTrusted, cfg.Workspace.SkillsReason = false, "off"
	}
}
