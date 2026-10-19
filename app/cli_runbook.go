package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// runbookMarkdown lists a runbook's steps with each call exactly as it will
// be made, so what the person approves is what runs.
func runbookMarkdown(rb *agent.Runbook) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Runbook · %d steps · plan %s\n\n", len(rb.Steps), shortPlanHash(rb.Hash))
	for _, s := range rb.Steps {
		head := fmt.Sprintf("%d. %s", s.Index, s.Tool)
		if s.Target != "" {
			head += " · " + ui.VisibleLine(s.Target)
		}
		if s.Note != "" {
			head += " — " + ui.VisibleLine(s.Note)
		}
		if s.Destructive != "" {
			head += " · NO UNDO: " + ui.VisibleLine(s.Destructive)
		}
		b.WriteString(head + "\n\n" + codeFence(stepText(s)) + "\n\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// stepText is what a step runs: the command, or the call's other arguments.
func stepText(s agent.RunbookStep) string {
	m, err := tools.DecodeArgs(s.Args)
	if err != nil {
		return string(s.Args)
	}
	if c, ok := m["command"].(string); ok && (s.Tool == "ssh" || s.Tool == "bash") && len(m) <= 3 {
		return ui.Visible(c)
	}
	for _, k := range []string{"host", "cluster", "namespace", "path"} {
		if v, _ := m[k].(string); v != "" && strings.Contains(s.Target, v) {
			delete(m, k)
		}
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return string(s.Args)
	}
	return ui.Visible(string(out))
}

// codeFence puts text in a code fence no backtick run inside it can close.
func codeFence(text string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + "\n" + text + "\n" + fence
}

func shortPlanHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// destructiveSteps are the steps with no undo.
func destructiveSteps(rb *agent.Runbook) []agent.RunbookStep {
	var out []agent.RunbookStep
	if rb == nil {
		return nil
	}
	for _, s := range rb.Steps {
		if s.Destructive != "" {
			out = append(out, s)
		}
	}
	return out
}

// runbookConfirm is the second question a runbook with steps that cannot be
// undone needs, as each of those calls would have asked on its own.
func runbookConfirm(steps []agent.RunbookStep) ui.DialogSpec {
	var b strings.Builder
	for _, s := range steps {
		fmt.Fprintf(&b, "- step %d, %s: %s\n", s.Index, strings.TrimSpace(s.Tool+" "+ui.VisibleLine(s.Target)), ui.VisibleLine(s.Destructive))
	}
	return ui.DialogSpec{
		Kind:  ui.DialogConfirm,
		Title: fmt.Sprintf("%d steps of this runbook cannot be undone", len(steps)),
		Body:  []ui.Block{{Kind: ui.BlockMarkdown, Text: b.String()}},
		Ask:   "Approve them with the plan? They will run without asking.",
		Choices: []ui.Choice{
			{ID: ui.ChoiceNo, Label: "No, keep planning"},
			{ID: ui.ChoiceYes, Label: "Yes, approve the steps that cannot be undone"},
		},
		Default: ui.ChoiceNo,
		Cancel:  ui.ChoiceNo,
	}
}

// runbookSpec is a runbook the operator supplied with -runbook.
type runbookSpec struct {
	text   string
	steps  []tools.RunbookStep
	hash   string // the plan hash it must have, "" for any
	prompt string // the steps as the model is given them, once bound
}

// maxRunbookFile bounds a -runbook file.
const maxRunbookFile = 1 << 20

// loadRunbookFile reads a runbook file: exit_plan's own arguments, the plan
// and its runbook, so a plan proposed in a conversation can be saved and run.
func loadRunbookFile(path, hash string) (*runbookSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRunbookFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRunbookFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxRunbookFile)
	}
	var doc struct {
		Plan    string              `json:"plan"`
		Runbook []tools.RunbookStep `json:"runbook"`
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Runbook) == 0 {
		return nil, errors.New(path + " has no runbook steps")
	}
	if strings.TrimSpace(doc.Plan) == "" {
		doc.Plan = "Runbook " + filepath.Base(path)
	}
	return &runbookSpec{text: doc.Plan, steps: doc.Runbook, hash: hash}, nil
}
