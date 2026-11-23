package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Parallel subagents, optionally each in its own git worktree.
//
// The single `task` tool runs one subagent and waits. This runs several at
// once — the shape of "investigate these three modules" or "try both fixes and
// tell me which passes". The interesting case is when they WRITE: two agents
// editing one checkout will overwrite each other, and the parent cannot tell
// whose change survived. Isolation gives each child its own worktree on its
// own branch; the parent gets back what changed and how to merge it, and
// nothing touches the main tree until a person decides it should.

// Tasks is the tool.
type Tasks struct {
	Spawn func(ctx context.Context, req SubagentRequest) (string, error)
	// Agents are the agent types this session offers; nil offers the built-in roles.
	Agents *Definitions
	// Models are the provider names a task may choose, offered when more than one.
	Models []string
	// Background starts children that outlive the call; nil offers no
	// background property.
	Background func(ctx context.Context, req SubagentRequest) (string, error)
	// Workspace is the parent's root; worktrees are created beneath it.
	Workspace string
	// MaxParallel bounds concurrency. Zero means all at once.
	MaxParallel int
}

// inGit reports whether worktree isolation can be offered: the workspace is
// in a git repository now, or is not known. It is asked each time the tool
// is described, so a git init during the session offers it from then on.
func (t Tasks) inGit() bool { return t.Workspace == "" || InGitRepo(t.Workspace) }

func (Tasks) Name() string  { return "tasks" }
func (Tasks) Mutates() bool { return false } // each child's tools are policed on their own

// MutatesCall is true for worktree isolation, which makes branches and
// checkouts on the host before any child runs: that asks, and plan mode refuses it.
// A role whose definition works in its own worktree does the same.
func (t Tasks) MutatesCall(raw json.RawMessage) bool {
	var a tasksArgs
	if json.Unmarshal(raw, &a) != nil {
		return false
	}
	if a.Isolation == "worktree" {
		return true
	}
	for _, tk := range a.Tasks {
		if def, ok := t.Agents.Get(tk.AgentType); ok && def.Isolation == "worktree" {
			return true
		}
	}
	return false
}

func (t Tasks) Description() string {
	iso := "Set isolation to \"worktree\" when subagents will edit files: each then works on " +
		"its own git branch in its own checkout, and you receive each branch's changes and how " +
		"to merge them."
	if !t.inGit() {
		iso = "Not a git repository, so no isolation \"worktree\": give subagents that edit separate files."
	}
	return "Run several subagents AT THE SAME TIME, each with a fresh context and a complete " +
		"self-contained prompt. " + parallelUse + " Also for independent subtasks: separate " +
		"areas, or alternative approaches. " + iso + " Returns every subagent's summary, in order."
}

func (t Tasks) Schema() json.RawMessage {
	return mustSchema(map[string]any{
		"type": "object",
		"properties": withModel(map[string]any{
			"tasks": map[string]any{
				"type": "array", "minItems": 1, "maxItems": 8,
				"items": map[string]any{
					"type": "object",
					"properties": withModel(map[string]any{
						"prompt":      map[string]any{"type": "string", "description": "Complete, self-contained task. The subagent sees none of this conversation."},
						"description": map[string]any{"type": "string", "description": "3-5 word label shown to the user."},
						"agent_type":  agentTypeSchema(t.Agents),
						"max_turns":   map[string]any{"type": "integer"},
					}, t.Models, false),
					"required": []string{"prompt", "description"},
				},
			},
			"isolation": isolationSchema(t.inGit()),
		}, nil, t.Background != nil),
		"required": []string{"tasks"},
	})
}

// isolationSchema is the isolation property: worktree only where git is.
func isolationSchema(git bool) map[string]any {
	if !git {
		return map[string]any{"type": "string", "enum": []string{"none"},
			"description": "Outside a git repository subagents share the workspace. Default none."}
	}
	return map[string]any{"type": "string", "enum": []string{"none", "worktree"},
		"description": "worktree: each subagent edits its own git branch in its own checkout. Required when subagents will change files. Default none."}
}

type tasksArgs struct {
	Tasks      []taskArgs `json:"tasks"`
	Isolation  string     `json:"isolation"`
	Background bool       `json:"background"`
}

type taskOutcome struct {
	index    int
	desc     string
	summary  string
	err      error
	worktree *worktree
}

type worktree struct {
	Dir    string
	Branch string
	Start  string // the commit it was made at
}

func (t Tasks) Run(ctx context.Context, _ *tools.Session, raw json.RawMessage) tools.Result {
	var a tasksArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return tools.Result{Content: fmt.Sprintf("Invalid arguments for tasks: %v", err), IsError: true}
	}
	if len(a.Tasks) == 0 {
		return tools.Result{Content: "tasks must name at least one task.", IsError: true}
	}
	if len(a.Tasks) > 8 {
		return tools.Result{Content: "tasks is limited to 8 subagents at once.", IsError: true}
	}
	// Every task is checked before any runs: an agent type this session does
	// not offer refuses the whole call, rather than running as another role.
	isolate := make([]bool, len(a.Tasks))
	anyIsolated := false
	for i, tk := range a.Tasks {
		if strings.TrimSpace(tk.Prompt) == "" {
			return tools.Result{Content: fmt.Sprintf("tasks[%d].prompt is required and must be self-contained.", i), IsError: true}
		}
		// The label is what the person sees on each subagent's card.
		if strings.TrimSpace(tk.Description) == "" {
			a.Tasks[i].Description = labelFrom(tk.Prompt)
		}
		if a.Tasks[i].AgentType == "" {
			a.Tasks[i].AgentType = "general"
		}
		def, ok := t.Agents.Get(a.Tasks[i].AgentType)
		if !ok {
			return unknownType(t.Agents, fmt.Sprintf("tasks[%d].agent_type", i), tk.AgentType)
		}
		// A definition's worktree is a default the call may tighten, never loosen.
		isolate[i] = a.Isolation == "worktree" || def.Isolation == "worktree"
		anyIsolated = anyIsolated || isolate[i]
	}

	// Background is all or nothing: a call that would pass the session's
	// limit is refused as a whole, before any worktree or child. A stop from
	// here on refuses what has not started.
	epoch := 0
	var held *slots
	if a.Background {
		if t.Background == nil {
			return tools.Result{Content: "this agent runs no background tasks; call tasks without background.", IsError: true}
		}
		b, _ := managerOf(ctx)
		epoch = b.stopEpoch()
		var err error
		if held, err = b.reserveN(len(a.Tasks)); err != nil {
			return tools.Result{Content: err.Error() + ".", IsError: true}
		}
		defer held.release()
	}

	if anyIsolated {
		if err := requireGitRepo(ctx, t.Workspace); err != nil {
			return tools.Result{Content: "isolation \"worktree\" needs the workspace to be a git " +
				"repository: " + err.Error() + ". Use isolation \"none\" and agent types that do not " +
				"work in a worktree, or initialise git first.",
				IsError: true}
		}
	}

	// Worktrees are created up front and serially: git's own locking makes
	// concurrent `worktree add` fragile, and a failure here should stop the
	// whole call before any subagent has spent a token.
	trees := make([]*worktree, len(a.Tasks))
	for i := range a.Tasks {
		if !isolate[i] {
			continue
		}
		wt, err := addWorktree(ctx, t.Workspace)
		if err != nil {
			for _, done := range trees {
				if done != nil {
					removeWorktree(context.Background(), t.Workspace, done)
				}
			}
			return tools.Result{Content: "could not create a worktree: " + err.Error(), IsError: true}
		}
		trees[i] = wt
	}

	if a.Background {
		return t.startAll(ctx, a, trees, epoch, held)
	}

	limit := t.MaxParallel
	if limit <= 0 || limit > len(a.Tasks) {
		limit = len(a.Tasks)
	}
	sem := make(chan struct{}, limit)
	outcomes := make([]taskOutcome, len(a.Tasks))
	var wg sync.WaitGroup
	for i, tk := range a.Tasks {
		wg.Add(1)
		go func(i int, tk taskArgs) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req := SubagentRequest{Prompt: tk.Prompt, Description: tk.Description,
				AgentType: tk.AgentType, MaxTurns: tk.MaxTurns, Model: tk.Model}
			wt := trees[i]
			if wt != nil {
				req.Workspace, req.worktree = wt.Dir, wt
			}
			summary, err := t.Spawn(ctx, req)
			outcomes[i] = taskOutcome{index: i, desc: tk.Description, summary: summary, err: err, worktree: wt}
		}(i, tk)
	}
	wg.Wait()

	var b strings.Builder
	failed := 0
	for _, o := range outcomes {
		fmt.Fprintf(&b, "## Task %d — %s\n", o.index+1, o.desc)
		if o.err != nil {
			failed++
			fmt.Fprintf(&b, "FAILED: %v\n\n", o.err)
		} else {
			b.WriteString(strings.TrimSpace(o.summary))
			b.WriteString("\n\n")
		}
		if o.worktree != nil {
			rel, _ := filepath.Rel(t.Workspace, o.worktree.Dir)
			b.WriteString(settleWorktree(ctx, t.Workspace, rel, o.worktree))
		}
	}
	if failed > 0 {
		fmt.Fprintf(&b, "[%d of %d tasks failed]\n", failed, len(outcomes))
	}
	return tools.Result{Content: b.String(), IsError: failed == len(outcomes)}
}

// labelFrom is a short label from a prompt, for a task given none.
func labelFrom(prompt string) string {
	words := strings.Fields(prompt)
	if len(words) > 5 {
		return strings.Join(words[:5], " ") + "…"
	}
	return strings.Join(words, " ")
}

// Which note a message gets. A denylist alone let fresh phrasings about code
// through each round, and so did words that merely appeared together, so the
// strong note now depends on the sentence's shape: the message must open with
// the reading or analysis work itself, after an optional "please", "can you"
// or "could you" — a reading verb, "fan out and" one, or subagents named as
// the doers ("use/using/with/via … subagents") — and no "to <reading verb>"
// or "so it/they/that <reading verb>" may follow a noun, since there the
// reading is what a program is to be changed to do. Any other mention of
// parallel work gets only a note asking the model to check which the person
// means. A message that names a change to code, CI, or the program's own
// concurrency gets neither: a note there sends the model the wrong way, and
// /parallel always asks outright.
const readingVerbs = `review(s|ing)?|audit(s|ing)?|check(s|ing)?|investigat(e|es|ing)|` +
	`look(ing)? at|read(s|ing)?|summari[sz](e|es|ing)|compar(e|es|ing)|research(es|ing)?|` +
	`analy[sz](e|es|ing)|explor(e|es|ing)|search(es|ing)?|find(s|ing)?|triag(e|es|ing)|test`

var (
	parallelPhrase = regexp.MustCompile(`(?i)\b(in[ -]parallel|at the same time|at once|simultaneously|` +
		`side[ -]by[ -]side|paralleli[sz]e|fan(ning)?[ -]out|en paralelo)\b|समानांतर`)
	subagentsNamed = regexp.MustCompile(`(?i)\bsub-?agents?\b`)
	// readingWork is work that reads or judges rather than changes.
	readingWork = regexp.MustCompile(`(?i)\b(` + readingVerbs + `)\b`)
	// readingLead is a message that opens with the reading work, or with
	// subagents named as the ones to do it.
	readingLead = regexp.MustCompile(`(?i)^\s*(please\s+|(can|could)\s+you\s+)?(` +
		`(` + readingVerbs + `)\b|fan(ning)?[ -]out\s+and\s+(` + readingVerbs + `)\b|` +
		`(use|using|with|via)\s+(\S+\s+){0,2}?sub-?agents\b)`)
	// leadSubagents is a message that opens with subagents as the doers.
	leadSubagents = regexp.MustCompile(`(?i)^\s*(please\s+|(can|could)\s+you\s+)?(use|using|with|via)\s+(\S+\s+){0,2}?sub-?agents\b`)
	// programToRead is a noun followed by what it is to read: "the crawler to
	// read", "the loader so it reads". The subagents a message opens with are
	// the doers, not a program.
	programToRead = regexp.MustCompile(`(?i)\b(\w[\w-]*)\s+(to|so\s+(it|they|that))\s+(` + readingVerbs + `)\b`)
	// codeWork names a change to code, the program's own concurrency, or a
	// library or file that does it; it suppresses both notes.
	codeWork = regexp.MustCompile(`(?i)\b(build|builds|building|deploy|deploys|deploying|implement|implements|` +
		`add|adds|make|makes|refactor\w*|write|rewrit\w*|code|app|apps|cluster|compose|docker|should|must|` +
		`creat\w*|design(s|ed)?\s+(a|an|the|how|our|my|your|new)|designing|enabl\w*|switch\w*|turn on|spawn\w*|` +
		`ci|etl|multiprocessing|github actions?|workflows?|jenkins|pre-commit|xdist|pytest-xdist|makefile|tox|nox|` +
		`race|races|racy|goroutines?|threads?|thread-?safe|threadpool|threadpoolexecutor|asyncio|async|await|` +
		`errgroup|rayon|mutex(es)?|locks?|scheduler|worker[ -]?pools?|for[ -]loop|gpus?|connections?|connecting|` +
		`test suite|tests in parallel|safe to (call|run|use))\b|promise\.all|` +
		`\b[\w-]+\.(py|go|js|ts|tsx|jsx|rs|java|rb|c|cc|cpp|h|cs|kt|swift)\b|(^|\s)(-p\s*\d|-{1,2}parallel\b)`)
	// programRunning says the parallel phrase describes how a program runs,
	// not how the work is to be done: it comes within three words of a verb
	// for running, after a why/whether/that/how/if clause, or with a sign of
	// the program's own concurrency. It turns the strong note into the one
	// that asks.
	programRunning = regexp.MustCompile(`(?i)\b(run|runs|running|ran|happen|happens|happening|execute|executes|executing|` +
		`fire|fires|fired|process|processes|processed|processing|fetch|fetches|fetched|fetching|query|queries|` +
		`queried|querying|dispatch|dispatches|dispatched|dispatching|load|loads|loaded|loading|deadlock|deadlocks)` +
		`(\s+\S+){0,3}\s+(` + phraseWords + `)`)
	clauseBefore = regexp.MustCompile(`(?i)\b(why|whether|that|how|if)\b.*\b(` + phraseWords + `)`)
	// changeSigns say the message also asks for a change to a program: the
	// phrase is "parallelize", a pronoun for the program stands right before
	// a reading verb or the phrase ("have it read", "parallelize its reads"),
	// or it speaks of performance ("speed up", "by reading").
	changeSigns = regexp.MustCompile(`(?i)paralleli[sz]|\b(it|its|them|they)\s+(` + readingVerbs + `|` + phraseWords + `)\b|` +
		`\b(speed(s|ing)? up|faster|slow|slower|performance)\b|\bby\s+\w+ing\b`)
	programSigns = regexp.MustCompile(`(?i)\b(deadlocks?|workers|jobs|requests|instead of|rather than|one by one|one after another|serially|sequentially)\b`)
)

// phraseWords are the parallel phrases, for the patterns that look at what
// comes before one.
const phraseWords = `in[ -]parallel|at the same time|at once|simultaneously|side[ -]by[ -]side`

// noteTier is which note the person's own words get.
type noteTier int

const (
	noNote noteTier = iota
	clarifyNote
	strongNote
)

// parallelTier judges the person's own words.
func parallelTier(typed string) noteTier {
	if codeWork.MatchString(typed) {
		return noNote
	}
	phrase := parallelPhrase.MatchString(typed)
	lead := leadSubagents.MatchString(typed)
	if (phrase || lead) && readingLead.MatchString(typed) && readingWork.MatchString(typed) &&
		!readingForAProgram(typed, lead) && !programRunning.MatchString(typed) &&
		!clauseBefore.MatchString(typed) && !programSigns.MatchString(typed) && !changeSigns.MatchString(typed) {
		return strongNote
	}
	if phrase || subagentsNamed.MatchString(typed) {
		return clarifyNote
	}
	return noNote
}

// readingForAProgram reports a "to <reading verb>" or "so it <reading verb>"
// after a noun, other than the subagents a message opens with.
func readingForAProgram(typed string, lead bool) bool {
	for _, m := range programToRead.FindAllStringSubmatch(typed, -1) {
		if lead && subagentsNamed.MatchString(m[1]) {
			continue
		}
		return true
	}
	return false
}

// ParallelNote is what Abhed adds after a message that asks for reading or
// analysis work in parallel. The tool descriptions say what such a request
// means, and a small local model still made the project's code concurrent;
// these words beside the message turned it to subagents, where conditional
// or softer wordings did not.
const ParallelNote = "[Abhed: this message asks for work in parallel. Unless it asks in so many words " +
	"to change the program to run concurrently, it means how you do the work: split it into " +
	"independent parts and start them all at once as subagents with one tasks call.]"

// ClarifyNote is what Abhed adds after any other mention of parallel work,
// where the words alone do not say which the person means.
const ClarifyNote = "[Abhed: this message mentions parallel work. If it is unclear whether the person " +
	"wants independent pieces of work started as subagents, or the program itself changed to run " +
	"concurrently, ask one short question before acting.]"

// UnattendedClarifyNote is the clarify note for a run nobody can answer
// (-p, rpc, a server's unattended run), given only to a vague request (see
// vagueSpeedRequest): the question cannot be asked, and a real model went
// straight to making the program concurrent. It stays conditional, as the
// note that asks does, so work the message does ask for is still done.
const UnattendedClarifyNote = "[Abhed: nobody can answer a question in this run. If this message only asks to " +
	"speed things up or to run things in parallel, and names no concrete change, it is unclear whether the person " +
	"wants independent pieces of work started as subagents or the program itself changed to run concurrently: " +
	"then make no code edits for it, and answer with a short plan that describes both readings, or with the one " +
	"question you would ask. Otherwise do the work it asks for.]"

var (
	// speedOrParallel names speed or parallelism itself.
	speedOrParallel = regexp.MustCompile(`(?i)\b(in[ -]parallel|at the same time|at once|simultaneously|side[ -]by[ -]side|` +
		`paralleli[sz]\w*|concurrent(ly)?|speed(s|ing)? up|faster|quicker|slow|slower|performance)\b`)
	// concreteTask is a task the message names: an action with an object, a
	// bug, or a file or symbol.
	concreteTask = regexp.MustCompile(`(?i)\b(fix|fixes|fixing|delete|deletes|deleting|remove|removes|removing|update|updates|updating|` +
		`bump|bumps|bumping|add|adds|adding|rename|renames|renaming|move|moves|moving|edit|edits|editing|change|changes|changing|` +
		`replace|replaces|replacing|migrate|migrates|migrating|install|installs|upgrade|upgrades|upgrading|write|writes|writing|` +
		`create|creates|creating|refactor|refactors|refactoring|review|reviews|reviewing|test|tests|testing|document|documents|` +
		`translate|convert|converts|format|formats|lint|clean|cleans|generate|generates|handle|handles|handling|` +
		`bug|bugs|crash|crashes|crashing|error|errors|fail|fails|failing|failure|broken|timeout|timeouts|regression|issue|issues)\b`)
	// namedThing is a file, a path or an identifier.
	namedThing = regexp.MustCompile(`[A-Za-z0-9_./-]*(?:[a-z][A-Z]|_[A-Za-z]|\.[A-Za-z]{1,5}\b|/[A-Za-z])[A-Za-z0-9_./-]*`)
)

// vagueSpeedRequest reports a message that names speed or parallelism and no
// concrete task: no action with an object, bug, file or symbol, and no object
// beyond generic ones ("things", "everything", "the project"). Only such a
// message gets the unattended note; "Delete these three files at once" and
// "Merge these three branches at once" are work to do. A list of verbs alone
// kept missing the next one, so the object decides too.
func vagueSpeedRequest(typed string) bool {
	return speedOrParallel.MatchString(typed) && !concreteTask.MatchString(typed) &&
		!namedThing.MatchString(typed) && !specificObject(typed)
}

var (
	// speedWords are stripped before objects are looked for, so "at the same
	// time" is not read as an object "same".
	speedWords = regexp.MustCompile(`(?i)\b(` + phraseWords + `|speed(s|ing)? up)\b`)
	objectWord = regexp.MustCompile(`[\p{L}\p{N}'-]+`)
	// determiners open a noun phrase whose head decides whether it is generic.
	determiners = wordSet("the these those my our your their all both each every " +
		"two three four five six seven eight nine ten eleven twelve several")
	// quantifiers may stand between a determiner and its head.
	quantifiers = wordSet("of the these those my our your their all both each every whole entire other " +
		"two three four five six seven eight nine ten eleven twelve several")
	// notAnObject heads are generic nouns or words that end the phrase with no
	// noun ("do these three at the same time", "run all in parallel").
	notAnObject = wordSet("thing things stuff everything anything work project projects repo repository " +
		"codebase program system whole rest it them they this that in at on to by for with and or but up " +
		"down out as so is are be same time way faster quicker slower more less much now please then " +
		"run runs running go do done speed performance")
)

// specificObject reports a noun phrase opened by a determiner or a count
// ("the three migrations", "these branches", "all three checks") whose head
// is not generic.
func specificObject(typed string) bool {
	words := objectWord.FindAllString(strings.ToLower(speedWords.ReplaceAllString(typed, " ")), -1)
	for i, w := range words {
		if !determiners[w] && !isNumber(w) {
			continue
		}
		j := i + 1
		for j < len(words) && (quantifiers[words[j]] || isNumber(words[j])) {
			j++
		}
		if j < len(words) && !notAnObject[words[j]] {
			return true
		}
	}
	return false
}

func isNumber(w string) bool {
	for _, r := range w {
		if r < '0' || r > '9' {
			return false
		}
	}
	return w != ""
}

// madeVague reports a message that codeWork passed over only for "make"
// ("Can you make everything run in parallel?"), with a parallel phrase and
// nothing more concrete: as vague as the request the unattended note is for.
func madeVague(typed string) bool {
	rest := makeWord.ReplaceAllString(typed, " ")
	return rest != typed && !codeWork.MatchString(rest) && parallelPhrase.MatchString(typed) && vagueSpeedRequest(typed)
}

var makeWord = regexp.MustCompile(`(?i)\bmakes?\b`)

// parallelNote is the note for the person's own words, typed, or "". Only a
// top-level loop that can still start a subagent with tasks adds it: a
// subagent's prompt is the model's writing, and a nested spawn is refused by
// default.
func (l *Loop) parallelNote(typed string) string {
	if typed == "" || l.depth != 0 || l.Tools == nil || strings.HasPrefix(typed, parallelLead) || !l.Budget.CanSpawn() {
		return ""
	}
	if _, ok := l.Tools.Get("tasks"); !ok {
		return ""
	}
	switch parallelTier(typed) {
	case strongNote:
		return ParallelNote
	case clarifyNote:
		// A question nobody can answer would end the run with it: an
		// unattended run gets a note only for a vague request, which it
		// answers with a plan or the question.
		if l.Unattended {
			if vagueSpeedRequest(typed) {
				return UnattendedClarifyNote
			}
			return ""
		}
		return ClarifyNote
	case noNote:
		if l.Unattended && madeVague(typed) {
			return UnattendedClarifyNote
		}
	}
	return ""
}

type typedKey struct{}

// WithTyped marks ctx's run as one the person asked for in typed, their own
// words without what they attached, for a caller that passes the message as
// one string with its attachments.
func WithTyped(ctx context.Context, typed string) context.Context {
	return context.WithValue(ctx, typedKey{}, typed)
}

func typedFrom(ctx context.Context) string {
	s, _ := ctx.Value(typedKey{}).(string)
	return s
}

// parallelLead opens /parallel's turn.
const parallelLead = "Run this work as parallel subagents, now."

// ParallelPrompt is the turn /parallel sends: the person's work, with the
// instruction to split it into subagents that run at the same time.
func ParallelPrompt(work string) string {
	return parallelLead + " Split it into its independent parts and " +
		"start them all in ONE tasks call: one entry per part, each with a complete, " +
		"self-contained prompt and a 3-5 word description. Do not do the parts yourself one " +
		"after another, and do not change the project's code to add parallelism. When they " +
		"return, report each part's result.\n\nThe work:\n" + strings.TrimSpace(work)
}

// startAll starts every task in the background, each settling its own
// worktree when it ends.
func (t Tasks) startAll(ctx context.Context, a tasksArgs, trees []*worktree, epoch int, held *slots) tools.Result {
	if testHookBeforeBackground != nil {
		testHookBeforeBackground()
	}
	var b strings.Builder
	failed := 0
	stopped := false
	for i, tk := range a.Tasks {
		req := SubagentRequest{Prompt: tk.Prompt, Description: tk.Description,
			AgentType: tk.AgentType, MaxTurns: tk.MaxTurns, Model: tk.Model, epoch: epoch, epochSet: true, slots: held}
		if wt := trees[i]; wt != nil {
			req.Workspace, req.settle, req.worktree = wt.Dir, settleLater(t.Workspace, wt), wt
		}
		fmt.Fprintf(&b, "## Task %d — %s\n", i+1, tk.Description)
		// After a stop, none of the rest starts.
		var id string
		err := ErrStopped
		if !stopped {
			id, err = t.Background(ctx, req)
		}
		stopped = stopped || errors.Is(err, ErrStopped)
		if err != nil {
			failed++
			if req.settle != nil {
				req.settle(context.WithoutCancel(ctx))
			}
			fmt.Fprintf(&b, "FAILED: %v\n\n", err)
			continue
		}
		b.WriteString(startedText(id, tk.Description) + "\n\n")
	}
	return tools.Result{Content: b.String(), IsError: failed == len(a.Tasks)}
}

// settle removes a worktree the subagent left as it was made, with its branch,
// and otherwise keeps it and says what it holds and how to take it.
func settleWorktree(ctx context.Context, ws, rel string, wt *worktree) string {
	untouched, err := hostgit.Untouched(ctx, wt.Dir, wt.Start)
	if err != nil {
		return fmt.Sprintf("Worktree %s (branch %s): its state could not be read (%v); it is kept.\n\n", rel, wt.Branch, err)
	}
	if untouched {
		removeWorktree(context.WithoutCancel(ctx), ws, wt)
		return fmt.Sprintf("Worktree %s (branch %s): no changes; removed with its branch.\n\n", rel, wt.Branch)
	}
	discard := fmt.Sprintf("To discard: `git worktree remove --force %s && git branch -D %s`.", rel, wt.Branch)
	if stat, n := worktreeChanges(ctx, wt.Dir); n > 0 {
		return fmt.Sprintf("Worktree %s (branch %s): %d file(s) changed, UNCOMMITTED.\n%s\n"+
			"To take these changes: `/take %s` in the terminal, or the workbench's Changes view, reviews them by hunk; "+
			"or review with `git -C %s diff`, commit there, then `git merge %s` from the main tree. %s\n\n",
			rel, wt.Branch, n, stat, filepath.Base(wt.Dir), rel, wt.Branch, discard)
	}
	return fmt.Sprintf("Worktree %s (branch %s): nothing uncommitted, but it holds commits or ignored files, "+
		"so it is kept. Review with `git log %s..%s`, then `git merge %s` from the main tree. %s\n\n",
		rel, wt.Branch, wt.Start[:min(12, len(wt.Start))], wt.Branch, wt.Branch, discard)
}

// ------------------------------------------------------------------ git

// git runs one command on r, whose drivers were read once for the operation.
func git(ctx context.Context, r *hostgit.Repo, args ...string) (string, error) {
	cmd := r.Command(ctx, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// InGitRepo reports whether ws or a folder above it holds .git, for what the
// tasks tool offers. It runs nothing: it is asked as every session starts,
// and running git there cost a second. The tool itself asks git.
func InGitRepo(ws string) bool {
	if ws == "" {
		return false
	}
	for d := filepath.Clean(ws); ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		up := filepath.Dir(d)
		if up == d {
			return false
		}
		d = up
	}
}

func requireGitRepo(ctx context.Context, ws string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git is not installed")
	}
	_, err := git(ctx, hostgit.New(ctx, ws), "rev-parse", "--git-dir")
	return err
}

// WorktreeDir is where isolated checkouts live: inside the workspace, and outside
// Abhed's state so the subagents can write there.
const WorktreeDir = tools.WorktreesDir

func addWorktree(ctx context.Context, ws string) (*worktree, error) {
	id := newID()
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	r := hostgit.New(ctx, ws)
	parent, err := r.Worktrees(ctx)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(parent, id)
	if err := hostgit.NewWorktreeDir(dir); err != nil {
		return nil, err
	}
	branch := "abhed/" + id
	if _, err := git(ctx, r, "worktree", "add", "-b", branch, dir, "HEAD"); err != nil {
		return nil, err
	}
	wt := &worktree{Dir: dir, Branch: branch}
	if err := r.Placed(ctx, dir); err != nil {
		_, _ = git(ctx, r, "branch", "-D", branch)
		return nil, err
	}
	start, err := hostgit.Head(ctx, dir)
	if err != nil {
		removeWorktree(ctx, ws, wt)
		return nil, err
	}
	wt.Start = start
	return wt, nil
}

func removeWorktree(ctx context.Context, ws string, wt *worktree) {
	r := hostgit.New(ctx, ws)
	_, _ = git(ctx, r, "worktree", "remove", "--force", wt.Dir)
	_, _ = git(ctx, r, "branch", "-D", wt.Branch)
	hostgit.RemoveWorktrees(ws)
}

// worktreeChanges summarises what a subagent left uncommitted: a diff stat
// and the number of changed paths, untracked included.
func worktreeChanges(ctx context.Context, dir string) (string, int) {
	r := hostgit.New(ctx, dir)
	status, err := git(ctx, r, "status", "--porcelain")
	if err != nil || status == "" {
		return "", 0
	}
	n := len(strings.Split(status, "\n"))
	stat, _ := git(ctx, r, "diff", "--stat", "HEAD")
	if stat == "" {
		stat = status
	}
	return stat, n
}
