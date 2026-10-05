package app

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/server"
	"golang.org/x/term"
)

// headlessOpts are how a -p run reads and writes.
type headlessOpts struct {
	format  string // text, json or stream-json
	partial bool   // stream-json: include agent.delta and reasoning deltas
	verbose bool
	schema  json.RawMessage
	// inputs are the user messages after the first, from -input-format
	// stream-json; nil for a single task.
	inputs <-chan string
	start  map[string]any
	// provider names the model, for an error that says what to do.
	providerName string
	provider     config.ProviderConfig
	// fallback, when set, records its moves in this run's record.
	fallback *fallbackAdapter
}

// maxStdin bounds the stdin a -p run takes as context.
const maxStdin = 10 << 20

// headlessTask is the task a -p run is given: the command line's, with
// piped stdin added as its input. With stdin alone, stdin is the task. A
// stop signal ends the wait for stdin.
func headlessTask(ctx context.Context, prompt string, stdin io.Reader, isPipe bool, wait io.Writer) (string, error) {
	if !isPipe {
		return prompt, nil
	}
	type read struct {
		data []byte
		err  error
	}
	got := make(chan read, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(stdin, maxStdin+1))
		got <- read{data, err}
	}()
	var r read
	// A pipe whose writer is slow, or never closes, would otherwise look
	// like a hang.
	select {
	case r = <-got:
	case <-time.After(3 * time.Second):
		fmt.Fprintln(wait, "abhed: reading the task's input from stdin until it ends (redirect from /dev/null to skip it)")
		select {
		case r = <-got:
		case <-ctx.Done():
			return "", context.Cause(ctx)
		}
	case <-ctx.Done():
		return "", context.Cause(ctx)
	}
	if r.err != nil {
		return "", fmt.Errorf("reading stdin: %w", r.err)
	}
	if len(r.data) > maxStdin {
		return "", fmt.Errorf("stdin is larger than %d MiB; pass a path in the task instead", maxStdin>>20)
	}
	in := strings.TrimRight(string(r.data), "\n")
	switch {
	case strings.TrimSpace(in) == "":
		return prompt, nil
	case prompt == "":
		return in, nil
	}
	return prompt + "\n\nInput from stdin:\n" + in, nil
}

// stdinIsPipe reports whether stdin is a pipe or a file rather than a
// terminal or a device such as /dev/null.
func stdinIsPipe() bool {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return false
	}
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice == 0
}

// streamInputs reads -input-format stream-json: one JSON object per line,
// {"type":"user","message":{"content":...}} with the content a string or a
// list of text parts. Other lines are reported and skipped.
func streamInputs(r io.Reader, warn io.Writer) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), maxStdin)
		for n := 1; sc.Scan(); n++ {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			text, err := userMessageText([]byte(line))
			if err != nil {
				fmt.Fprintf(warn, "abhed: stdin line %d skipped: %v\n", n, err)
				continue
			}
			out <- text
		}
	}()
	return out
}

func userMessageText(line []byte) (string, error) {
	var m struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return "", fmt.Errorf("not JSON: %w", err)
	}
	if m.Type != "user" {
		return "", fmt.Errorf("type %q; only user messages are read", m.Type)
	}
	raw := m.Message.Content
	if len(raw) == 0 {
		raw = m.Content
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b []string
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				b = append(b, p.Text)
			}
		}
		if len(b) > 0 {
			return strings.Join(b, "\n"), nil
		}
	}
	return "", errors.New("no text content")
}

// maxSchema bounds a -json-schema file: a schema is small, and @/dev/zero
// must not hang the run.
const maxSchema = 1 << 20

// schemaFlag reads -json-schema: inline JSON, or @path, relative to the
// workspace. It must be a JSON object, as a schema is.
func schemaFlag(v, workspace string) (json.RawMessage, error) {
	if v == "" {
		return nil, nil
	}
	data := []byte(v)
	if strings.HasPrefix(v, "@") {
		path := v[1:]
		if !filepath.IsAbs(path) {
			path = filepath.Join(workspace, path)
		}
		f, err := os.Open(path) // #nosec G304 -- a file the person named on the command line
		if err != nil {
			return nil, fmt.Errorf("-json-schema: %w", err)
		}
		defer f.Close()
		if data, err = io.ReadAll(io.LimitReader(f, maxSchema+1)); err != nil {
			return nil, fmt.Errorf("-json-schema: %w", err)
		}
		if len(data) > maxSchema {
			return nil, fmt.Errorf("-json-schema: %s is larger than 1 MiB", path)
		}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		return nil, errors.New("-json-schema is not a JSON object")
	}
	return json.RawMessage(data), nil
}

// systemPrompt is what the command line does to the system prompt.
type systemPrompt struct {
	appendText, replaceText string
}

// systemPromptFlags reads the flags that change the system prompt. A
// replacement is refused under a managed configuration, whose
// instructions the prompt carries.
func systemPromptFlags(f *cliFlags, cfg config.Config) (systemPrompt, error) {
	var sp systemPrompt
	read := func(flagName, path string) (string, error) {
		b, err := os.ReadFile(path) // #nosec G304 -- a file the person named on the command line
		if err != nil {
			return "", fmt.Errorf("-%s: %w", flagName, err)
		}
		return string(b), nil
	}
	sp.appendText = f.appendSystem
	if f.appendFile != "" {
		t, err := read("append-system-prompt-file", f.appendFile)
		if err != nil {
			return sp, err
		}
		sp.appendText = strings.TrimSpace(sp.appendText + "\n\n" + t)
	}
	sp.replaceText = f.systemPrompt
	if f.systemFile != "" {
		if f.systemPrompt != "" {
			return sp, errors.New("-system-prompt and -system-prompt-file are both set; use one")
		}
		t, err := read("system-prompt-file", f.systemFile)
		if err != nil {
			return sp, err
		}
		sp.replaceText = t
	}
	if strings.TrimSpace(sp.replaceText) != "" && cfg.Managed {
		return sp, errors.New("-system-prompt is refused under a managed configuration, whose instructions the prompt carries; -append-system-prompt adds to it instead")
	}
	return sp, nil
}

// apply returns the system prompt with the flags applied.
func (sp systemPrompt) apply(base string) string {
	out := base
	if strings.TrimSpace(sp.replaceText) != "" {
		out = sp.replaceText
	}
	if strings.TrimSpace(sp.appendText) != "" {
		out = strings.TrimRight(out, "\n") + "\n\n" + sp.appendText
	}
	return out
}

// record puts into the session.started payload what the flags did to the
// system prompt: digests of what they supplied, never the text.
func (sp systemPrompt) record(into map[string]any) {
	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}
	into["system_prompt"] = "default"
	if strings.TrimSpace(sp.replaceText) != "" {
		into["system_prompt"] = "replaced"
		into["system_prompt_sha256"] = sum(sp.replaceText)
	}
	if strings.TrimSpace(sp.appendText) != "" {
		into["system_prompt_appended_sha256"] = sum(sp.appendText)
	}
}

// resumedStart is start for a run that continues a recorded conversation
// after seq after: marked resumed, with the step it goes on from.
func resumedStart(start map[string]any, after int64) map[string]any {
	if start == nil || after == 0 {
		return start
	}
	out := maps.Clone(start)
	out["resumed"], out["through_seq"] = true, after
	return out
}

// recordedMode is the permission mode a record was left in: the last
// mode.changed, or else the mode the last session.started names.
func recordedMode(events []agent.Event) string {
	mode := ""
	for _, ev := range agent.Live(events) {
		switch ev.Type {
		case agent.EvSessionStarted:
			var p struct {
				Mode string `json:"mode"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil && p.Mode != "" {
				mode = p.Mode
			}
		case agent.EvModeChanged:
			var m agent.ModeChanged
			if json.Unmarshal(ev.Payload, &m) == nil {
				mode = m.To
			}
		}
	}
	return mode
}

// recordResumedMode records a mode.changed when a continued run's mode is
// not the one its record was left in, so the record never shows the old one.
func recordResumedMode(rec *agent.Recorder, events []agent.Event, now, via string) {
	if was := recordedMode(events); was != "" && now != "" && was != now {
		if _, err := rec.Record(agent.EvModeChanged, agent.ActorUser, agent.Trusted, agent.ModeChanged{
			From: was, To: now, By: agent.ByUser, Via: via,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: recording the mode: %v\n", err)
		}
	}
}

// recordStart records how the session was started. The CLI recorded
// nothing before the first message, so a changed system prompt left no
// trace in the record.
func recordStart(rec *agent.Recorder, start map[string]any) {
	if start == nil {
		return
	}
	if _, err := rec.Record(agent.EvSessionStarted, agent.ActorSystem, agent.Trusted, start); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: recording the session start: %v\n", err)
	}
}

// resultLine is the last line json and stream-json write: how the run ended.
type resultLine struct {
	Type       string          `json:"type"` // "result"
	Subtype    string          `json:"subtype"`
	IsError    bool            `json:"is_error"`
	Result     string          `json:"result"`
	Structured json.RawMessage `json:"structured_output,omitempty"`
	Error      string          `json:"error,omitempty"`
	SessionID  string          `json:"session_id"`
	NumTurns   int             `json:"num_turns"`
	DurationMS int64           `json:"duration_ms"`
	ExitCode   int             `json:"exit_code"`
	Usage      resultUsage     `json:"usage"`
	// Omitted names the event types this output left out, so a reader of the
	// capture can tell a gap where they sat from a missing event.
	Omitted []agent.EventType `json:"omitted,omitempty"`
}

type resultUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CachedTokens int `json:"cached_tokens"`
}

func runOnce(ctx context.Context, store server.EventStore, r *ui.Renderer, o headlessOpts,
	adapter model.Adapter, registry *tools.Registry, pol *policy.Engine,
	approver agent.Approver, sess *tools.Session, cfg agent.Config,
	appCfg config.Config, prompt string, budget *agent.Budget, extHost *extension.Host) int {

	began := time.Now()
	// A new session, or with -c or -r the recorded one it goes on with.
	sessionID, seed, err := headlessSession(ctx, &cliState{store: store, appCfg: appCfg, workspace: sess.Root, adapter: adapter})
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1 // a run with no session row would write into another's record
	}
	rec := agent.NewRecorder(store, sessionID, "")
	// Read again as the store changes: bash reads it at each call.
	rec.Redact = openVault().Session()
	streaming := o.format != "text"
	quietText := !streaming && o.schema != nil

	events := store.Subscribe(sessionID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			if o.verbose && ev.Type == agent.EvModelCall {
				fmt.Fprintf(os.Stderr, "abhed: model call %s\n", ev.Payload)
			}
			switch {
			case streaming:
				if o.format == "stream-json" && !o.partial &&
					(ev.Type == agent.EvAgentDelta || ev.Type == agent.EvAgentReasoningDelta) {
					continue
				}
				b, _ := json.Marshal(ev)
				fmt.Println(string(b))
			case !quietText:
				r.Event(ev)
			}
		}
	}()
	if o.fallback != nil {
		o.fallback.SetRecord(recordFallback(rec))
	}

	loop := agent.NewLoop(adapter, registry, pol, approver, sess, rec, cfg)
	loop.Provider = appCfg.Model.Default
	// The factory's budget, so the subagents' spend and the loop's are one.
	loop.Budget = budget
	seed(loop)
	// After the seed, so a continued session's start follows its record.
	resumedAfter := rec.LastAppended()
	var before []agent.Event
	if resumedAfter > 0 {
		before, _ = store.Events(sessionID)
	}
	recordStart(rec, resumedStart(o.start, resumedAfter))
	if mode, _ := o.start["mode"].(string); resumedAfter > 0 {
		recordResumedMode(rec, before, mode, resumedVia(mode, appCfg))
	}
	if startFlags.Name != "" {
		_, _ = loop.Recorder.Record(agent.EvSessionNamed, agent.ActorUser, agent.Trusted, agent.SessionNamed{Name: startFlags.Name})
	}
	// Nobody comes back to a -p run, so its background tasks are joined: the
	// run, and the exit code, wait for them.
	agent.NewBackground(loop, toolset.BackgroundPolicy(appCfg, agent.WakeOff))
	defer loop.Background.Close(agent.TermSessionClosed)
	if appCfg.Sets("subagents.wake") && appCfg.Subagents.Wake != "off" {
		fmt.Fprintf(os.Stderr, "abhed: note: subagents.wake is %s, but -p runs background tasks joined: it waits for them\n", config.Printable(appCfg.Subagents.Wake))
	}
	loop.Compactor = agent.NewCompactor(adapter, cfg.CompactAt)
	toolset.Summarize(loop.Compactor, extHost, sessionID)
	if extHost != nil && extHost.Len() > 0 {
		recordFired(extHost, func() *agent.Recorder { return rec })
	}

	var (
		reason     agent.TerminalReason
		structured json.RawMessage
		ran        bool
	)
	runOne := func(task string) {
		ran = true
		if o.schema != nil {
			structured, reason, err = agent.RunStructured(ctx, loop, loop.Tools, task, o.schema)
			return
		}
		reason, err = loop.Run(ctx, task)
	}
	if prompt != "" {
		runOne(prompt)
	}
	// The next message is awaited alongside a stop signal: stdin may stay
	// open for as long as the caller likes. Reading stops after a failure.
	for o.inputs != nil && err == nil && (!ran || reason.ExitCode() == 0) && ctx.Err() == nil {
		task, open := "", false
		select {
		case task, open = <-o.inputs:
		case <-ctx.Done():
		}
		if !open {
			break
		}
		runOne(task)
	}
	if !ran && err == nil {
		err = errors.New("no task: stdin held no user message")
	}

	store.Unsubscribe(sessionID, events)
	<-done

	code := reason.ExitCode()
	var noResult agent.ErrNoResult
	switch {
	case errors.As(err, &noResult):
		code = max(noResult.Reason.ExitCode(), 1)
	case err != nil:
		code = agent.TermError.ExitCode()
	}
	// A stop signal ends the run as a shell reports it: 130, 143, 129.
	if c, stopped := stopCode(ctx); stopped {
		code = c
	}

	u := loop.Usage()
	switch {
	case streaming:
		res := resultLine{Type: "result", Subtype: string(reason), IsError: code != 0,
			Result: runAnswer(loop.Messages(), store, sessionID, resumedAfter), Structured: structured,
			SessionID: sessionID, NumTurns: u.Turns, DurationMS: time.Since(began).Milliseconds(), ExitCode: code,
			Usage: resultUsage{u.InputTokens, u.OutputTokens, u.CachedTokens}}
		if res.Subtype == "" {
			res.Subtype = string(agent.TermError)
		}
		if o.format == "stream-json" && !o.partial {
			res.Omitted = []agent.EventType{agent.EvAgentDelta, agent.EvAgentReasoningDelta}
		}
		if err != nil {
			res.Error = friendlyModelError(err, o.providerName, o.provider)
		}
		b, _ := json.Marshal(res)
		fmt.Println(string(b))
	case quietText:
		if structured != nil {
			fmt.Println(string(structured))
		}
	default:
		printUsage(r, u)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %s\n", friendlyModelError(err, o.providerName, o.provider))
	}
	if code != 0 {
		noteIgnoredModel(appCfg)
	}
	return code
}

// lastAssistantText is the model's last non-empty reply.
// runAnswer is this run's last answer: a continued session's earlier answer
// is not, though its conversation holds it, so a run stopped before answering
// says nothing rather than repeat the last run's.
func runAnswer(msgs []model.Message, es server.EventStore, id string, after int64) string {
	if after > 0 {
		evs, err := es.Events(id)
		if err != nil || !slices.ContainsFunc(evs, func(e agent.Event) bool { return e.Seq > after && e.Type == agent.EvAgentMessage }) {
			return ""
		}
	}
	return lastAssistantText(msgs)
}

func lastAssistantText(msgs []model.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleAssistant && strings.TrimSpace(msgs[i].Content) != "" {
			return msgs[i].Content
		}
	}
	return ""
}

func printUsage(r *ui.Renderer, u agent.Usage) {
	s := r.Style()
	line := fmt.Sprintf("%d turns · %d in / %d out tokens", u.Turns, u.InputTokens, u.OutputTokens)
	// Cache hit rate is a UX metric as much as a capacity one (docs P8), so it
	// is shown rather than hidden in telemetry.
	if u.InputTokens > 0 && u.CachedTokens > 0 {
		line += fmt.Sprintf(" · %.0f%% cached (%.1fx prefill)", u.CacheHitRate()*100, u.PrefillSavings())
	}
	if u.Compactions > 0 {
		line += fmt.Sprintf(" · %d compaction(s)", u.Compactions)
	}
	fmt.Printf("%s\n", s.Dim(line))
}
