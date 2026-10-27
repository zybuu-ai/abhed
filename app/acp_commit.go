package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/secretfiles"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A commit message for what is staged (studio-acp-contract.md §7.12): the
// engine reads the staged diff itself, through the confined git, leaves out
// files the session may not read, withholds secrets, and asks the commit
// model for a message. Studio sends no diff.

func init() {
	liveFeatures = append(liveFeatures, "commitMessage")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/commitMessage": (*acpConn).commitMessage,
	})
}

// Commit message bounds: the diff sent, the answer, and the wait.
const (
	commitDiffBytes = 32 << 10
	commitMaxTokens = 2048 // room for a reasoning model to think before it writes
	commitTimeout   = 90 * time.Second
)

// stagedDiff is what of the staged change may be sent: the diff of the files
// the session may read, cleaned and cut to size, and how many were left out.
type stagedDiff struct {
	text            string
	files, excluded int
	truncated       bool
}

// readStaged reads the staged change under root through the confined git;
// with staged false, the working tree's unstaged changes to tracked files.
func readStaged(ctx context.Context, root string, roots []string, mayRead func(string) bool, clean func(string) string, staged bool) (stagedDiff, error) {
	which := []string{"diff", "--cached"}
	if !staged {
		which = []string{"diff"}
	}
	names, err := hostgit.Command(ctx, root, append(which, "--relative", "--name-only", "-z")...).Output()
	if err != nil {
		return stagedDiff{}, err
	}
	var d stagedDiff
	var keep []string
	for _, name := range strings.Split(strings.TrimRight(string(names), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(name))
		if sendablePath(path, roots) != "" || !mayRead(path) || secretfiles.Match(path) != "" || tools.IsState(path, roots...) {
			d.excluded++
			continue
		}
		keep = append(keep, ":(literal)"+name)
	}
	d.files = len(keep)
	if len(keep) == 0 {
		return d, nil
	}
	out, err := hostgit.Command(ctx, root, append(append(which, "--relative", "--no-color", "--stat", "--patch", "-U3", "--"), keep...)...).Output()
	if err != nil {
		return stagedDiff{}, err
	}
	text := clean(string(bytes.ToValidUTF8(out, []byte("?"))))
	if len(text) > commitDiffBytes {
		text, d.truncated = headBytes(text, commitDiffBytes), true
	}
	d.text = text
	return d, nil
}

func (c *acpConn) commitMessage(msg rpcMessage) {
	start := time.Now()
	outcome := "refused"
	// Every request ends logged, with how it ended, so a silent one is never silent.
	defer func() {
		fmt.Fprintf(os.Stderr, "abhed: acp: commit message: %s in %s\n", outcome, time.Since(start).Round(time.Millisecond))
	}()
	fail := func(e *rpcError) {
		outcome = "error: " + e.Message
		c.reply(msg.ID, nil, e)
	}
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		fail(e)
		return
	}
	parts, e := innerOf(s)
	if e != nil {
		fail(e)
		return
	}
	empty := func(field, why string) {
		outcome = field + ": " + why
		c.reply(msg.ID, map[string]any{"message": "", field: why}, nil)
	}
	cfg := parts.Config
	off, provider := editorOff(cfg, "commit")
	if off != "" {
		empty("disabled", off)
		return
	}
	var spare bucket
	if !c.compRateTake(completeNow(), &spare) {
		empty("limited", "too many requests; slow down")
		return
	}
	a, err := completionAdapter(provider, cfg.Completion.LocalOnly)
	if err != nil {
		fail(refusal(errRefused, "the commit model cannot be used: %v", err))
		return
	}
	// Ended by the session's close or the connection's, as well as the timeout.
	k := c.completerFor(s, "commit")
	if k == nil {
		fail(refusal(errParams, "the session has closed"))
		return
	}
	ctx, cancel := context.WithTimeout(c.root(), commitTimeout)
	defer cancel()
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		fail(refusal(errParams, "the session has closed"))
		return
	}
	if k.cancel != nil {
		k.cancel() // a newer request replaces one still waiting
	}
	k.cancel = cancel
	k.mu.Unlock()
	clean := func(text string) string {
		text = parts.Loop.Withheld(text)
		text, _ = secretfiles.RedactContent(text)
		return text
	}
	roots := append([]string{parts.Session.Root}, s.roots()...)
	mayRead := func(p string) bool { return !readDenied(parts.Loop.Policy, p) }
	diff, err := readStaged(ctx, parts.Session.Root, roots, mayRead, clean, true)
	source := "staged"
	if err == nil && diff.files == 0 && diff.excluded == 0 {
		// Nothing staged: the message is for the changes in the working tree.
		diff, err = readStaged(ctx, parts.Session.Root, roots, mayRead, clean, false)
		source = "unstaged"
	}
	if err != nil {
		fail(refusal(errRefused, "the changes could not be read: %s", redacted(err.Error())))
		return
	}
	if diff.files == 0 {
		why := "nothing is staged or changed"
		if diff.excluded > 0 {
			why = "every changed file is one the session may not read"
		}
		empty("empty", why)
		return
	}
	name, _ := cfg.EditorModel("commit")
	// The record says where the diff goes before it goes; if it cannot, nothing is sent.
	if _, err := parts.Loop.Recorder.Record(agent.EvCommitRequested, agent.ActorUser, agent.Trusted, agent.CommitRequested{
		Provider: name, Model: provider.Model, Local: provider.OnThisMachine(),
		Files: diff.files, Excluded: diff.excluded, Bytes: len(diff.text), Truncated: diff.truncated,
	}); err != nil {
		fail(refusal(errRefused, "the record did not take the request, so nothing was sent"))
		return
	}
	res, err := model.CommitMessageByChat(ctx, a, diff.text, commitMaxTokens)
	text := res.Text
	if clean(text) != text {
		text, res.Dropped = "", "the message carried a secret"
	}
	recorded := "served"
	switch {
	case err != nil:
		recorded = "failed"
	case strings.TrimSpace(text) == "":
		recorded = "empty"
	}
	if _, rerr := parts.Loop.Recorder.Record(agent.EvCommitMessage, agent.ActorSystem, agent.Trusted,
		agent.CommitMessage{Outcome: recorded, TokensIn: res.InputTokens, TokensOut: res.OutputTokens}); rerr != nil {
		warnf("the record did not take %s: %v", agent.EvCommitMessage, rerr)
	}
	switch {
	case err != nil && ctx.Err() != nil:
		fail(refusal(errRefused, "the commit model did not answer within %s", commitTimeout))
	case err != nil:
		fail(refusal(errRefused, "the commit model did not answer: %s", redacted(err.Error())))
	case recorded == "empty":
		// A clear error, not an empty answer: the person asked for a message.
		why := orDefault(res.Dropped, "the commit model gave no message")
		if res.Dropped == "" && res.OutputTokens >= commitMaxTokens {
			why += " (it spent its tokens before writing one)"
		}
		fail(refusal(errRefused, "%s", why))
	default:
		outcome = "served"
		out := map[string]any{"message": text, "source": source}
		if diff.excluded > 0 || diff.truncated {
			out["excluded"], out["truncated"] = diff.excluded, diff.truncated
		}
		c.reply(msg.ID, out, nil)
	}
}
