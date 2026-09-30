package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Limits on what one mention attaches, so a mention of a generated file
// cannot fill the context by itself.
const (
	mentionMaxBytes = 256 << 10
	mentionMaxLines = 20000
)

// mentionRe finds "@path" words: at the start or after whitespace, so an
// email address is not a mention. A quoted form allows spaces.
var mentionRe = regexp.MustCompile(`(^|\s)@("[^"\n]+"|[^\s"]+)`)

// rangeRe is a line range after a path: file:10, file:10-20 or file#L10-20.
var rangeRe = regexp.MustCompile(`^(.+?)(?::|#L)(\d+)(?:-L?(\d+))?$`)

// mentionRef is one @ mention as typed.
type mentionRef struct {
	Raw      string // the text after @
	Path     string
	From, To int // 1-based and inclusive; 0 for the whole file
}

// Range is the lines as input.mention records them, "" for the whole file.
func (m mentionRef) Range() string {
	switch {
	case m.From == 0:
		return ""
	case m.To == m.From:
		return strconv.Itoa(m.From)
	}
	return fmt.Sprintf("%d-%d", m.From, m.To)
}

// parseMentions finds the mentions in text, each once.
func parseMentions(text string) []mentionRef {
	var out []mentionRef
	seen := map[string]bool{}
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		raw := m[2]
		if strings.HasPrefix(raw, `"`) {
			raw = strings.Trim(raw, `"`)
		} else {
			raw = strings.TrimRight(raw, ".,;!?)'")
		}
		if raw == "" || seen[raw] {
			continue
		}
		seen[raw] = true
		out = append(out, parseMentionRef(raw))
	}
	return out
}

// parseMentionRef splits a mention into its path and line range. A range
// that does not read as one is left as part of the path.
func parseMentionRef(raw string) mentionRef {
	ref := mentionRef{Raw: raw, Path: strings.TrimSuffix(raw, ":")}
	m := rangeRe.FindStringSubmatch(ref.Path)
	if m == nil {
		return ref
	}
	from, err := strconv.Atoi(m[2])
	if err != nil || from < 1 {
		return ref
	}
	to := from
	if m[3] != "" {
		if to, err = strconv.Atoi(m[3]); err != nil || to < from {
			return ref
		}
	}
	ref.Path, ref.From, ref.To = m[1], from, to
	return ref
}

// mentionExpander attaches the files and directories a message mentions,
// read as the agent's own read and glob calls would be: through the
// session's policy, inside its roots, never through a link that leaves them
// or into Abhed's state, redacted and recorded as the person's action.
type mentionExpander struct {
	st *cliState
}

var _ InputExpander = mentionExpander{}

// Expand attaches each mention. A mention of a path that does not exist is
// left as the text it is. One that exists but may not be read refuses the
// whole message, with each reason, so nothing is sent without what the person
// meant to attach.
func (m mentionExpander) Expand(ctx context.Context, loop *agent.Loop, raw string) (agent.Message, []Attachment, error) {
	refs := parseMentions(raw)
	if len(refs) == 0 {
		return agent.Message{Text: raw}, nil, nil
	}
	if loop == nil || loop.Session == nil {
		return agent.Message{}, nil, errors.New("there is no conversation to attach files to")
	}
	sess := loop.Session
	var blocks, refused []string
	var atts []Attachment
	for _, ref := range refs {
		abs := mentionPath(sess, ref.Path)
		info, err := os.Lstat(abs)
		if err != nil {
			continue // not a path: an @handle or a typo stays as text
		}
		isDir := info.IsDir()
		if info.Mode()&os.ModeSymlink != 0 {
			// Where it leads decides; the read below refuses one that leaves.
			if target, err := os.Stat(abs); err == nil {
				isDir = target.IsDir()
			}
		}
		var block string
		var att Attachment
		var why string
		if isDir {
			block, att, why, err = attachDir(ctx, loop, sess, abs)
		} else {
			block, att, why, err = attachFile(ctx, loop, sess, abs, ref)
		}
		if err != nil {
			return agent.Message{}, nil, err
		}
		if why != "" {
			refused = append(refused, fmt.Sprintf("@%s: %s", ref.Raw, why))
			continue
		}
		blocks = append(blocks, block)
		atts = append(atts, att)
	}
	if len(refused) > 0 {
		return agent.Message{}, nil, errors.New(strings.Join(refused, "\n"))
	}
	for _, a := range atts {
		if _, err := loop.Recorder.Record(agent.EvInputMention, agent.ActorUser, agent.Trusted, a.Mention()); err != nil {
			return agent.Message{}, nil, err
		}
	}
	if len(blocks) == 0 {
		return agent.Message{Text: raw}, nil, nil
	}
	text := raw + "\n\nFiles the person attached with @, read through the session's policy:\n" + strings.Join(blocks, "\n")
	return agent.Message{Text: text}, atts, nil
}

// mentionPath is the absolute path a mention names: ~/ is the home
// directory, and a relative path is the workspace's.
func mentionPath(sess *tools.Session, p string) string {
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	case !filepath.IsAbs(p):
		p = filepath.Join(sess.Root, filepath.FromSlash(p))
	}
	return filepath.Clean(p)
}

// attachFile reads one file through the read tool as the person. why is the
// refusal when it may not be read; err is a failure to record.
func attachFile(ctx context.Context, loop *agent.Loop, sess *tools.Session, abs string, ref mentionRef) (block string, att Attachment, why string, err error) {
	args := map[string]any{"path": abs, "limit": mentionMaxLines}
	if ref.From > 0 {
		args["offset"], args["limit"] = ref.From, ref.To-ref.From+1
	}
	res, err := loop.Manual(ctx, sess, "read", personCallID("mention"), argsJSON(args))
	if err != nil {
		return "", Attachment{}, "", err
	}
	if res.IsError {
		return "", Attachment{}, strings.TrimSpace(res.Content), nil
	}
	content, truncated := capText(res.Content, mentionMaxBytes)
	note := ""
	if truncated {
		note = fmt.Sprintf("\n[attached the first %d KB; read the file for the rest]", mentionMaxBytes>>10)
	}
	rel := filepath.ToSlash(sess.Rel(abs))
	lines := ""
	if r := ref.Range(); r != "" {
		lines = fmt.Sprintf(" lines=%q", r)
	}
	sum := sha256.Sum256([]byte(content))
	att = Attachment{Path: rel, Range: ref.Range(), SHA256: hex.EncodeToString(sum[:]),
		Bytes: int64(len(content)), Truncated: truncated || res.Truncated}
	return fmt.Sprintf("<file path=%q%s>\n%s%s\n</file>", rel, lines, content, note), att, "", nil
}

// attachDir lists a directory's files through the glob tool as the person,
// after the read rules for the directory itself.
func attachDir(ctx context.Context, loop *agent.Loop, sess *tools.Session, abs string) (block string, att Attachment, why string, err error) {
	id := personCallID("mention")
	dirArgs := argsJSON(map[string]string{"path": abs + string(filepath.Separator)})
	if d := loop.Policy.Evaluate("read", false, dirArgs); d.Decision == policy.Deny {
		if err := loop.ManualRefused("read", id, dirArgs, d); err != nil {
			return "", Attachment{}, "", err
		}
		return "", Attachment{}, "Denied: " + d.Reason, nil
	}
	globArgs := argsJSON(map[string]string{"pattern": "**", "path": abs})
	tool, refusal, err := loop.ManualAuthorize("glob", id, globArgs)
	if err != nil {
		return "", Attachment{}, "", err
	}
	if refusal != nil {
		return "", Attachment{}, strings.TrimSpace(refusal.Content), nil
	}
	start := time.Now()
	res := tool.Run(ctx, sess, globArgs)
	res.Content = redactFor(loop, res.Content)
	if err := loop.ManualObserve(id, "glob", res, time.Since(start)); err != nil {
		return "", Attachment{}, "", err
	}
	if res.IsError {
		return "", Attachment{}, strings.TrimSpace(res.Content), nil
	}
	content, truncated := capText(res.Content, mentionMaxBytes)
	rel := filepath.ToSlash(sess.Rel(abs))
	sum := sha256.Sum256([]byte(content))
	att = Attachment{Path: rel + "/", SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(content)),
		Truncated: truncated || res.Truncated}
	return fmt.Sprintf("<directory path=%q>\n%s\n</directory>", rel+"/", content), att, "", nil
}

// capText cuts text to at most n bytes at a line or character boundary.
func capText(text string, n int) (string, bool) {
	if len(text) <= n {
		return text, false
	}
	cut := text[:n]
	if i := strings.LastIndexByte(cut, '\n'); i > n/2 {
		cut = cut[:i]
	}
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}

// Completion of @ mentions. The terminal UI renders the list; this is where
// the entries come from: the repository's files and their directories, or a
// bounded walk outside a repository, matched fuzzily, with anything a read
// could not reach (Abhed's state, a link that leaves the workspace) left out.

// mentionIndexMax bounds the file index, so a huge tree cannot stall typing.
const mentionIndexMax = 20000

// mentionIndexTTL is how long a file index is reused between keystrokes.
const mentionIndexTTL = 5 * time.Second

type mentionIndex struct {
	mu    sync.Mutex
	root  string
	at    time.Time
	files []string
}

var mentionFiles mentionIndex

// mentionCandidates lists up to limit completions for "@"+prefix, best first.
func mentionCandidates(ctx context.Context, sess *tools.Session, prefix string, limit int) []string {
	if sess == nil || limit <= 0 {
		return nil
	}
	all := mentionFiles.get(ctx, sess.Root)
	type scored struct {
		path  string
		score int
	}
	var hits []scored
	for _, f := range all {
		if s, ok := fuzzyScore(f, prefix); ok {
			hits = append(hits, scored{f, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].path < hits[j].path
	})
	var out []string
	for _, h := range hits {
		if len(out) == limit {
			break
		}
		abs := filepath.Join(sess.Root, filepath.FromSlash(strings.TrimSuffix(h.path, "/")))
		if _, err := sess.Resolve(abs); err != nil {
			continue
		}
		out = append(out, h.path)
	}
	return out
}

func (x *mentionIndex) get(ctx context.Context, root string) []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.root == root && time.Since(x.at) < mentionIndexTTL {
		return x.files
	}
	x.root, x.at, x.files = root, time.Now(), indexFiles(ctx, root)
	return x.files
}

// indexFiles lists a workspace's files, relative and slash-separated, then
// the directories that hold them with a trailing slash.
func indexFiles(ctx context.Context, root string) []string {
	var files []string
	cmd := hostgit.New(ctx, root).Command(ctx, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if out, err := cmd.Output(); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(out))
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
			if i := bytes.IndexByte(data, 0); i >= 0 {
				return i + 1, data[:i], nil
			}
			if atEOF && len(data) > 0 {
				return len(data), data, nil
			}
			return 0, nil, nil
		})
		for sc.Scan() && len(files) < mentionIndexMax {
			files = append(files, sc.Text())
		}
	} else {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // an unreadable entry is skipped
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", tools.StateDir, tools.WorktreesDir:
					if p != root {
						return filepath.SkipDir
					}
				}
				return nil
			}
			if rel, err := filepath.Rel(root, p); err == nil {
				files = append(files, filepath.ToSlash(rel))
			}
			if len(files) >= mentionIndexMax {
				return filepath.SkipAll
			}
			return nil
		})
	}
	dirs := map[string]bool{}
	for _, f := range files {
		for d := pathDir(f); d != "" && !dirs[d]; d = pathDir(d) {
			dirs[d] = true
		}
	}
	for d := range dirs {
		files = append(files, d+"/")
	}
	sort.Strings(files)
	return files
}

// pathDir is a slash path's parent, "" at the top.
func pathDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return ""
}

// fuzzyScore matches query as a subsequence of path, ignoring case. A match
// in the file name, a prefix and a contiguous run score higher.
func fuzzyScore(path, query string) (int, bool) {
	if query == "" {
		return 0, true
	}
	p, q := strings.ToLower(path), strings.ToLower(query)
	base := p[strings.LastIndexByte(strings.TrimSuffix(p, "/"), '/')+1:]
	score := 0
	switch {
	case strings.HasPrefix(p, q):
		score += 300
	case strings.HasPrefix(base, q):
		score += 200
	case strings.Contains(p, q):
		score += 100
	}
	i, run := 0, 0
	for j := 0; j < len(p) && i < len(q); j++ {
		if p[j] == q[i] {
			i++
			run++
			score += run
		} else {
			run = 0
		}
	}
	if i < len(q) {
		return 0, false
	}
	return score - len(p)/8, true
}
