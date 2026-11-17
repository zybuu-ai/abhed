package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// The workspace's files for an editor whose engine runs on another machine
// (Studio on an SSH host; docs/architecture/studio-remote.md). The editor has
// no file system of its own there: it lists, reads, writes, searches and
// watches through these methods, so the person's file operations meet the
// same rules as the agent's. Only paths inside the session's roots are
// served; Abhed's own state is never served, and a path the read rules
// withhold is neither listed nor read. A write goes through the write tool on
// the manual path and is recorded by: user; a delete through the delete rules;
// a rename is judged as a delete of the old name and a write of the new.

func init() {
	liveFeatures = append(liveFeatures, "fs")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/fs/stat":    (*acpConn).fsStat,
		"_abhed/fs/readDir": (*acpConn).fsReadDir,
		"_abhed/fs/read":    (*acpConn).fsRead,
		"_abhed/fs/write":   (*acpConn).fsWrite,
		"_abhed/fs/delete":  (*acpConn).fsDelete,
		"_abhed/fs/rename":  (*acpConn).fsRename,
		"_abhed/fs/mkdir":   (*acpConn).fsMkdir,
		"_abhed/fs/search":  (*acpConn).fsSearch,
		"_abhed/fs/files":   (*acpConn).fsFiles,
		"_abhed/fs/watch":   (*acpConn).fsWatch,
		"_abhed/fs/unwatch": (*acpConn).fsUnwatch,
		"_abhed/fs/git":     (*acpConn).fsGit,
	})
}

const (
	// fsMaxRead bounds one file read or written through the editor.
	fsMaxRead = 16 << 20
	// fsMaxEntries bounds a listing, a walk for search or files, and a watch.
	fsMaxEntries = 20000
	// fsMaxSearchFile skips larger files in a text search.
	fsMaxSearchFile = 1 << 20
	// fsMaxWatches bounds the watches one connection keeps.
	fsMaxWatches = 64
	// fsWatchEvery is how often a watch looks.
	fsWatchEvery = 1500 * time.Millisecond
)

// fsErr codes a refusal for the editor's file system: notFound, exists,
// notADirectory, isADirectory, noPermissions.
func fsErr(code int, kind, format string, args ...any) *rpcError {
	e := refusal(code, format, args...)
	e.Data = map[string]any{"fs": kind}
	return e
}

// fsSession is the session a file request names, which must be one the engine runs.
func (c *acpConn) fsSession(msg rpcMessage) (*acpSession, *rpcError) {
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		return nil, e
	}
	if _, e := innerOf(s); e != nil {
		return nil, e
	}
	return s, nil
}

// fsPath is a requested path inside the session's roots, resolved; a path
// outside them is refused as not found, so nothing is said of what is there.
func fsPath(s *acpSession, p string) (string, *rpcError) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", refusal(errParams, "path is an absolute path in the workspace")
	}
	abs, ok := s.within(p)
	if !ok {
		return "", fsErr(errPolicy, "notFound", "%s is not in this session's workspace", p)
	}
	return abs, nil
}

// fsWithheld says why a path may not be read: Abhed's own state, or a read rule.
func fsWithheld(s *acpSession, abs string) string {
	if tools.IsState(abs, s.parts.Session.PolicyRoots()...) {
		return "the path is Abhed's own state"
	}
	if d := s.parts.Loop.Policy.Evaluate("read", false, argsJSON(map[string]string{"path": abs})); d.Decision == policy.Deny {
		return d.Reason
	}
	return ""
}

// fsReadable is fsPath for a read, refusing what fsWithheld names.
func fsReadable(s *acpSession, p string) (string, *rpcError) {
	abs, e := fsPath(s, p)
	if e != nil {
		return "", e
	}
	if why := fsWithheld(s, abs); why != "" {
		return "", fsErr(errPolicy, "noPermissions", "%s may not be read: %s", p, why)
	}
	return abs, nil
}

// fsName is a path to act on by its own name, not what it resolves to: the
// folder it is in must lie inside the session's roots, and the name itself is
// kept, so a delete or rename of a symbolic link acts on the link, never on
// its target.
func fsName(s *acpSession, p string) (string, *rpcError) {
	if p == "" || strings.ContainsRune(p, 0) || !filepath.IsAbs(p) {
		return "", refusal(errParams, "path is an absolute path in the workspace")
	}
	clean := filepath.Clean(p)
	name := filepath.Base(clean)
	parent, ok := s.within(filepath.Dir(clean))
	if !ok || name == "." || name == ".." || name == string(filepath.Separator) {
		return "", fsErr(errPolicy, "notFound", "%s is not in this session's workspace", p)
	}
	full := filepath.Join(parent, name)
	if _, ok := s.within(parent); !ok {
		return "", fsErr(errPolicy, "notFound", "%s is not in this session's workspace", p)
	}
	return full, nil
}

// personRemove deletes one entry by its own name (a link, not its target) as
// the person: through the delete rules, refused for Abhed's state, and
// recorded as their edit with what it held.
func (s *acpSession) personRemove(path string) error {
	loop, id := s.parts.Loop, "u"+acpID()
	args, _ := json.Marshal(map[string]string{"path": path})
	if tools.IsState(path, s.parts.Session.PolicyRoots()...) {
		d := policy.Result{Decision: policy.Deny, Reason: "the path is Abhed's own state", Step: "state"}
		_ = loop.ManualRefused("delete", id, args, d)
		return errors.New("Denied: " + d.Reason)
	}
	refused, err := loop.ManualCheck("delete", id, args)
	if err != nil {
		return err
	}
	if refused != nil {
		_ = loop.ManualObserve(id, "delete", *refused, 0)
		return errors.New(refused.Content)
	}
	sum := ""
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		if b, err := os.ReadFile(path); err == nil { // #nosec G304 -- inside the session's roots, a regular file
			sum = hashHex(b)
		}
	}
	start := time.Now()
	if err := os.Remove(path); err != nil {
		_ = loop.ManualObserve(id, "delete", tools.Result{Content: err.Error(), IsError: true}, time.Since(start))
		return err
	}
	if err := loop.ManualObserve(id, "delete", tools.Result{Content: "removed " + path}, time.Since(start)); err != nil {
		return err
	}
	s.record(agent.EvManualEdit, agent.ManualEdit{Path: path, BeforeSHA256: sum, By: agent.ByUser})
	return nil
}

type fsParams struct {
	Path      string `json:"path"`
	To        string `json:"to,omitempty"`
	Content   string `json:"content,omitempty"`
	Create    bool   `json:"create,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	ID        string `json:"id,omitempty"`
}

func fsParamsOf(msg rpcMessage) fsParams {
	var p fsParams
	_ = json.Unmarshal(msg.Params, &p)
	return p
}

// fsStatOf is a file's type, size and times as the editor reads them, in milliseconds.
func fsStatOf(info fs.FileInfo) map[string]any {
	kind := 1 // file
	if info.IsDir() {
		kind = 2
	}
	return map[string]any{"type": kind, "size": info.Size(), "mtime": info.ModTime().UnixMilli(), "ctime": info.ModTime().UnixMilli()}
}

func (c *acpConn) fsStat(msg rpcMessage) {
	s, e := c.fsSession(msg)
	if e == nil {
		var abs string
		if abs, e = fsReadable(s, fsParamsOf(msg).Path); e == nil {
			info, err := os.Stat(abs)
			if err != nil {
				e = fsErr(errRefused, "notFound", "%s does not exist", fsParamsOf(msg).Path)
			} else {
				c.reply(msg.ID, fsStatOf(info), nil)
				return
			}
		}
	}
	c.reply(msg.ID, nil, e)
}

func (c *acpConn) fsReadDir(msg rpcMessage) {
	s, e := c.fsSession(msg)
	var abs string
	if e == nil {
		abs, e = fsReadable(s, fsParamsOf(msg).Path)
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		c.reply(msg.ID, nil, fsErr(errRefused, "notADirectory", "%s is not a folder that can be read", fsParamsOf(msg).Path))
		return
	}
	out := []any{}
	for _, d := range entries {
		if len(out) >= fsMaxEntries {
			break
		}
		child := filepath.Join(abs, d.Name())
		// Listed only when it could be read: a withheld name is not shown at all.
		if real, ok := s.within(child); !ok || fsWithheld(s, real) != "" {
			continue
		}
		kind := 1
		if info, err := os.Stat(child); err == nil && info.IsDir() {
			kind = 2
		} else if err != nil {
			continue
		}
		out = append(out, map[string]any{"name": d.Name(), "type": kind})
	}
	c.reply(msg.ID, map[string]any{"entries": out}, nil)
}

func (c *acpConn) fsRead(msg rpcMessage) {
	s, e := c.fsSession(msg)
	var abs string
	if e == nil {
		abs, e = fsReadable(s, fsParamsOf(msg).Path)
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	info, err := os.Stat(abs)
	switch {
	case err != nil:
		e = fsErr(errRefused, "notFound", "%s does not exist", fsParamsOf(msg).Path)
	case info.IsDir():
		e = fsErr(errRefused, "isADirectory", "%s is a folder", fsParamsOf(msg).Path)
	case info.Size() > fsMaxRead:
		e = refusal(errRefused, "%s is over %d MiB", fsParamsOf(msg).Path, fsMaxRead>>20)
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	data, err := s.parts.Session.ReadFile(abs)
	if err != nil {
		c.reply(msg.ID, nil, fsErr(errRefused, "notFound", "%s could not be read", fsParamsOf(msg).Path))
		return
	}
	c.reply(msg.ID, map[string]any{"content": base64.StdEncoding.EncodeToString(data), "sha256": hashHex(data)}, nil)
}

// fsWrite saves a file as the person: through the write tool on the manual
// path, so the rules, the syntax check and the record apply as to any write.
func (c *acpConn) fsWrite(msg rpcMessage) {
	p := fsParamsOf(msg)
	s, e := c.fsSession(msg)
	var abs string
	if e == nil {
		abs, e = fsPath(s, p.Path)
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	data, err := base64.StdEncoding.DecodeString(p.Content)
	switch {
	case err != nil:
		e = refusal(errParams, "content is base64")
	case len(data) > fsMaxRead:
		e = refusal(errRefused, "a file over %d MiB is not written here", fsMaxRead>>20)
	case !utf8.Valid(data):
		e = refusal(errRefused, "only text is written over this connection; %s is binary", p.Path)
	}
	if e == nil {
		info, err := os.Stat(abs)
		switch {
		case err == nil && info.IsDir():
			e = fsErr(errRefused, "isADirectory", "%s is a folder", p.Path)
		case err == nil && !p.Overwrite:
			e = fsErr(errRefused, "exists", "%s exists", p.Path)
		case err != nil && !p.Create:
			e = fsErr(errRefused, "notFound", "%s does not exist", p.Path)
		}
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if err := s.manualWrite(c.root(), s.parts, abs, string(data)); err != nil {
		c.reply(msg.ID, nil, fsErr(errPolicy, "noPermissions", "%v", err))
		return
	}
	c.reply(msg.ID, map[string]any{"sha256": hashHex(data)}, nil)
}

// fsDelete removes a file, or a folder's files one by one, each through the
// delete rules and recorded by: user.
func (c *acpConn) fsDelete(msg rpcMessage) {
	p := fsParamsOf(msg)
	s, e := c.fsSession(msg)
	var abs string
	if e == nil {
		abs, e = fsName(s, p.Path)
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	info, err := os.Lstat(abs)
	if err != nil {
		c.reply(msg.ID, nil, fsErr(errRefused, "notFound", "%s does not exist", p.Path))
		return
	}
	if !info.IsDir() {
		if err := s.personRemove(abs); err != nil {
			c.reply(msg.ID, nil, fsErr(errPolicy, "noPermissions", "%v", err))
			return
		}
		c.reply(msg.ID, map[string]any{}, nil)
		return
	}
	if !p.Recursive {
		if err := os.Remove(abs); err != nil {
			c.reply(msg.ID, nil, refusal(errRefused, "%s is a folder that is not empty", p.Path))
			return
		}
		c.reply(msg.ID, map[string]any{}, nil)
		return
	}
	var files, dirs []string
	walkErr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if len(files)+len(dirs) > fsMaxEntries {
			return errors.New("the folder is too large to delete from the editor")
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		} else {
			files = append(files, path)
		}
		return nil
	})
	if walkErr != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "%v", walkErr))
		return
	}
	for _, f := range files {
		if err := s.personRemove(f); err != nil {
			c.reply(msg.ID, nil, fsErr(errPolicy, "noPermissions", "%s: %v", f, err))
			return
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
	c.reply(msg.ID, map[string]any{}, nil)
}

// fsRename is judged as a delete of the old name and a write of the new, for
// the entry and, for a folder, every entry inside it (with the read rules, so
// a withheld file is not moved out from under them). It acts on the names,
// so renaming a link renames the link.
func (c *acpConn) fsRename(msg rpcMessage) {
	p := fsParamsOf(msg)
	s, e := c.fsSession(msg)
	var from, to string
	if e == nil {
		from, e = fsName(s, p.Path)
	}
	if e == nil {
		to, e = fsName(s, p.To)
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	info, err := os.Lstat(from)
	if err != nil {
		c.reply(msg.ID, nil, fsErr(errRefused, "notFound", "%s does not exist", p.Path))
		return
	}
	if _, err := os.Lstat(to); err == nil && !p.Overwrite {
		c.reply(msg.ID, nil, fsErr(errRefused, "exists", "%s exists", p.To))
		return
	}
	type move struct{ from, to string }
	moves := []move{{from, to}}
	if info.IsDir() {
		walkErr := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == from {
				return nil
			}
			if len(moves) > fsMaxEntries {
				return errors.New("the folder is too large to rename from the editor")
			}
			rel, _ := filepath.Rel(from, path)
			moves = append(moves, move{path, filepath.Join(to, rel)})
			return nil
		})
		if walkErr != nil {
			c.reply(msg.ID, nil, refusal(errRefused, "%v", walkErr))
			return
		}
	}
	loop, id := s.parts.Loop, "u"+acpID()
	roots := s.parts.Session.PolicyRoots()
	for _, m := range moves {
		for _, j := range []struct{ action, path string }{{"delete", m.from}, {"write", m.to}, {"read", m.from}} {
			args := argsJSON(map[string]string{"path": j.path})
			d := loop.Policy.Evaluate(j.action, j.action != "read", args)
			if d.Decision != policy.Deny && tools.IsState(j.path, roots...) {
				d = policy.Result{Decision: policy.Deny, Reason: "the path is Abhed's own state", Step: "state"}
			}
			if d.Decision == policy.Deny {
				_ = loop.ManualRefused(j.action, id, args, d)
				c.reply(msg.ID, nil, fsErr(errPolicy, "noPermissions", "Denied (%s): %s", j.path, d.Reason))
				return
			}
		}
	}
	sum := ""
	if info.Mode().IsRegular() {
		if before, err := os.ReadFile(from); err == nil { // #nosec G304 -- inside the session's roots, a regular file
			sum = hashHex(before)
		}
	}
	if err := os.Rename(from, to); err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "%v", err))
		return
	}
	s.record(agent.EvManualEdit, agent.ManualEdit{Path: from, BeforeSHA256: sum, By: agent.ByUser})
	s.record(agent.EvManualEdit, agent.ManualEdit{Path: to, AfterSHA256: sum, By: agent.ByUser})
	c.reply(msg.ID, map[string]any{}, nil)
}

func (c *acpConn) fsMkdir(msg rpcMessage) {
	p := fsParamsOf(msg)
	s, e := c.fsSession(msg)
	var abs string
	if e == nil {
		abs, e = fsPath(s, p.Path)
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	args := argsJSON(map[string]string{"path": abs})
	d := s.parts.Loop.Policy.Evaluate("write", true, args)
	if d.Decision != policy.Deny && tools.IsState(abs, s.parts.Session.PolicyRoots()...) {
		d = policy.Result{Decision: policy.Deny, Reason: "the path is Abhed's own state", Step: "state"}
	}
	if d.Decision == policy.Deny {
		_ = s.parts.Loop.ManualRefused("write", "u"+acpID(), args, d)
		c.reply(msg.ID, nil, fsErr(errPolicy, "noPermissions", "Denied: %s", d.Reason))
		return
	}
	if err := os.Mkdir(abs, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			c.reply(msg.ID, nil, fsErr(errRefused, "exists", "%s exists", p.Path))
			return
		}
		c.reply(msg.ID, nil, refusal(errRefused, "%v", err))
		return
	}
	c.reply(msg.ID, map[string]any{}, nil)
}

// fsSkipDir is a folder a walk does not enter: version control and Abhed's state.
func fsSkipDir(name string) bool {
	return name == ".git" || name == ".hg" || name == ".svn" || strings.EqualFold(name, tools.StateDir) || name == "node_modules"
}

// fsWalk visits the readable files under root, relative paths with / separators,
// at most fsMaxEntries; it reports whether it stopped early.
func fsWalk(ctx context.Context, s *acpSession, root string, include, exclude []string, visit func(abs, rel string) bool) bool {
	n := 0
	stopped := false
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p != root && (fsSkipDir(d.Name()) || globAny(exclude, rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || globAny(exclude, rel) || (len(include) > 0 && !globAny(include, rel)) {
			return nil
		}
		if n++; n > fsMaxEntries {
			stopped = true
			return filepath.SkipAll
		}
		if fsWithheld(s, p) != "" {
			return nil
		}
		if !visit(p, rel) {
			stopped = true
			return filepath.SkipAll
		}
		return nil
	})
	return stopped
}

// globAny matches rel against simple globs: * and ? within a name, ** across
// folders, and a bare name anywhere in the path.
func globAny(globs []string, rel string) bool {
	for _, g := range globs {
		g = strings.TrimPrefix(strings.TrimSpace(g), "./")
		if g == "" {
			continue
		}
		if globMatch(g, rel) || (!strings.Contains(g, "/") && globMatch(g, path.Base(rel))) || globMatch(g+"/**", rel) {
			return true
		}
	}
	return false
}

func globMatch(g, rel string) bool {
	re := regexp.QuoteMeta(g)
	re = strings.ReplaceAll(re, `\*\*/`, `(?:.*/)?`)
	re = strings.ReplaceAll(re, `\*\*`, `.*`)
	re = strings.ReplaceAll(re, `\*`, `[^/]*`)
	re = strings.ReplaceAll(re, `\?`, `[^/]`)
	ok, _ := regexp.MatchString("^"+re+"$", rel)
	return ok
}

type fsSearchParams struct {
	Path          string   `json:"path"`
	Query         string   `json:"query"`
	IsRegex       bool     `json:"isRegex"`
	CaseSensitive bool     `json:"caseSensitive"`
	WholeWord     bool     `json:"wholeWord"`
	Include       []string `json:"include"`
	Exclude       []string `json:"exclude"`
	MaxResults    int      `json:"maxResults"`
}

// fsSearch is a text search over the readable files under path, line by line.
func (c *acpConn) fsSearch(msg rpcMessage) {
	var p fsSearchParams
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.fsSession(msg)
	var root string
	if e == nil {
		root, e = fsReadable(s, p.Path)
	}
	if e == nil && (p.Query == "" || len(p.Query) > 1000) {
		e = refusal(errParams, "query is 1 to 1000 characters")
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	expr := p.Query
	if !p.IsRegex {
		expr = regexp.QuoteMeta(expr)
	}
	if p.WholeWord {
		expr = `\b(?:` + expr + `)\b`
	}
	if !p.CaseSensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errParams, "the pattern does not compile: %v", err))
		return
	}
	limit := p.MaxResults
	if limit <= 0 || limit > 20000 {
		limit = 20000
	}
	ctx, cancel := context.WithTimeout(c.root(), 30*time.Second)
	defer cancel()
	matches := []any{}
	count := 0
	stopped := fsWalk(ctx, s, root, p.Include, p.Exclude, func(abs, rel string) bool {
		info, err := os.Stat(abs)
		if err != nil || info.Size() > fsMaxSearchFile {
			return true
		}
		data, err := os.ReadFile(abs) // #nosec G304 -- a readable file inside the session's roots
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return true
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64<<10), fsMaxSearchFile)
		for line := 0; sc.Scan(); line++ {
			text := sc.Text()
			locs := re.FindAllStringIndex(text, 50)
			if len(locs) == 0 {
				continue
			}
			ranges := [][2]int{}
			for _, l := range locs {
				// Columns in UTF-16 code units, as the editor counts them.
				ranges = append(ranges, [2]int{utf16Len(text[:l[0]]), utf16Len(text[:l[1]])})
			}
			if len(text) > 2000 {
				text = text[:2000]
			}
			matches = append(matches, map[string]any{"path": rel, "line": line, "text": text, "ranges": ranges})
			if count += len(locs); count >= limit {
				return false
			}
		}
		return true
	})
	c.reply(msg.ID, map[string]any{"matches": matches, "limitHit": stopped}, nil)
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// fsFiles lists the readable files under path, for quick open.
func (c *acpConn) fsFiles(msg rpcMessage) {
	var p fsSearchParams
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.fsSession(msg)
	var root string
	if e == nil {
		root, e = fsReadable(s, p.Path)
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	needle := strings.ToLower(p.Query)
	files := []string{}
	ctx, cancel := context.WithTimeout(c.root(), 20*time.Second)
	defer cancel()
	stopped := fsWalk(ctx, s, root, p.Include, p.Exclude, func(_, rel string) bool {
		if needle == "" || fuzzyContains(strings.ToLower(rel), needle) {
			files = append(files, rel)
		}
		return p.MaxResults <= 0 || len(files) < p.MaxResults
	})
	c.reply(msg.ID, map[string]any{"files": files, "limitHit": stopped}, nil)
}

// fuzzyContains reports whether the needle's characters appear in order in s.
func fuzzyContains(s, needle string) bool {
	i := 0
	for _, r := range s {
		if i < len(needle) && r == rune(needle[i]) {
			i++
		}
	}
	return i >= len(needle)
}

// fsWatches are a connection's watches, each a poll of a tree's times and sizes.
type fsWatches struct {
	mu   sync.Mutex
	byID map[string]context.CancelFunc
}

var fsWatchState sync.Map // *acpConn -> *fsWatches

func (c *acpConn) watches() *fsWatches {
	w, loaded := fsWatchState.LoadOrStore(c, &fsWatches{byID: map[string]context.CancelFunc{}})
	if !loaded {
		// Forgotten with the connection; each watch ends with its context.
		go func() { <-c.root().Done(); fsWatchState.Delete(c) }()
	}
	return w.(*fsWatches)
}

type fsSnap struct {
	size  int64
	mtime int64
	dir   bool
}

// fsSnapshot is the readable entries under root (root itself when it is a file).
func fsSnapshot(s *acpSession, root string, recursive bool) map[string]fsSnap {
	out := map[string]fsSnap{}
	info, err := os.Stat(root)
	if err != nil {
		return out
	}
	if !info.IsDir() {
		out[root] = fsSnap{info.Size(), info.ModTime().UnixNano(), false}
		return out
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || len(out) >= fsMaxEntries {
			return filepath.SkipDir
		}
		if p != root && d.IsDir() && (fsSkipDir(d.Name()) || !recursive) {
			if !recursive && !fsSkipDir(d.Name()) {
				if i, err := d.Info(); err == nil {
					out[p] = fsSnap{0, i.ModTime().UnixNano(), true}
				}
			}
			return filepath.SkipDir
		}
		if p == root || fsWithheld(s, p) != "" {
			return nil
		}
		if i, err := d.Info(); err == nil {
			out[p] = fsSnap{i.Size(), i.ModTime().UnixNano(), d.IsDir()}
		}
		return nil
	})
	return out
}

// fsWatch polls a path for changes and sends _abhed/fs/changed {id, changes:
// [{path, type}]} with type 1 changed, 2 added, 3 deleted.
func (c *acpConn) fsWatch(msg rpcMessage) {
	p := fsParamsOf(msg)
	s, e := c.fsSession(msg)
	var abs string
	if e == nil {
		abs, e = fsReadable(s, p.Path)
	}
	if e == nil && (p.ID == "" || len(p.ID) > 64) {
		e = refusal(errParams, "id names the watch")
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	w := c.watches()
	w.mu.Lock()
	if _, ok := w.byID[p.ID]; ok || len(w.byID) >= fsMaxWatches {
		w.mu.Unlock()
		c.reply(msg.ID, nil, refusal(errRefused, "watch %s exists, or there are %d already", p.ID, fsMaxWatches))
		return
	}
	ctx, cancel := context.WithCancel(c.root())
	w.byID[p.ID] = cancel
	w.mu.Unlock()
	go func() {
		prev := fsSnapshot(s, abs, p.Recursive)
		for {
			// A large tree is looked at less often: every 1.5 s up to 2,000 entries, then in proportion, at most every 30 s.
			wait := fsWatchEvery
			if n := len(prev); n > 2000 {
				wait = min(fsWatchEvery*time.Duration(n/2000+1), 30*time.Second)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			next := fsSnapshot(s, abs, p.Recursive)
			changes := []any{}
			for k, v := range next {
				if old, ok := prev[k]; !ok {
					changes = append(changes, map[string]any{"path": k, "type": 2})
				} else if old != v && !v.dir {
					changes = append(changes, map[string]any{"path": k, "type": 1})
				}
			}
			for k := range prev {
				if _, ok := next[k]; !ok {
					changes = append(changes, map[string]any{"path": k, "type": 3})
				}
			}
			prev = next
			if len(changes) > 0 {
				if len(changes) > 1000 {
					changes = changes[:1000]
				}
				c.notification("_abhed/fs/changed", map[string]any{"id": p.ID, "changes": changes})
			}
		}
	}()
	c.reply(msg.ID, map[string]any{}, nil)
}

func (c *acpConn) fsUnwatch(msg rpcMessage) {
	p := fsParamsOf(msg)
	w := c.watches()
	w.mu.Lock()
	if cancel, ok := w.byID[p.ID]; ok {
		cancel()
		delete(w.byID, p.ID)
	}
	w.mu.Unlock()
	c.reply(msg.ID, map[string]any{}, nil)
}

// gitFlags are the flags every git run for the SCM view takes: no hooks, no
// fsmonitor, no pager, no external diff, no submodules, no transport, no
// optional locks, so a repository's own configuration starts as little as
// git allows. The run is in the session's sandbox, as the agent's git is,
// so a filter or driver it still starts is held there.
var gitFlags = []string{"-c", "core.fsmonitor=", "-c", "core.hooksPath=/dev/null", "-c", "core.pager=cat",
	"-c", "diff.external=", "-c", "core.untrackedCache=false", "-c", "submodule.recurse=false", "-c", "protocol.allow=never",
	"--no-pager", "--no-optional-locks"}

// gitLine is a git command line for the sandbox: every GIT_* variable unset
// first (so none can point git at another repository or add configuration),
// then git with gitFlags and args, each quoted.
func gitLine(args ...string) string {
	words := []string{"git"}
	for _, a := range append(append([]string{}, gitFlags...), args...) {
		words = append(words, shellQuote(a))
	}
	return `for v in $(env | sed -n 's/^\(GIT_[A-Za-z0-9_]*\)=.*/\1/p'); do unset "$v"; done; ` +
		`GIT_CONFIG_NOSYSTEM=1 GIT_OPTIONAL_LOCKS=0 GIT_TERMINAL_PROMPT=0 GIT_ALLOW_PROTOCOL=abhed-none exec ` + strings.Join(words, " ")
}

// fsGit is the SCM view's read-only git: op status (porcelain v1, NUL
// separated, submodules ignored) or op original (the file as HEAD has it),
// run in the session's sandbox. Nothing is staged, committed or changed.
// With no sandbox there is nothing to hold a filter in, so it is refused.
func (c *acpConn) fsGit(msg rpcMessage) {
	var p struct {
		Op   string `json:"op"`
		Path string `json:"path"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.fsSession(msg)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	root := s.cwd
	var args []string
	switch p.Op {
	case "status":
		args = []string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=all"}
	case "original":
		abs, e := fsReadable(s, p.Path)
		if e != nil {
			c.reply(msg.ID, nil, e)
			return
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || !filepath.IsLocal(rel) {
			c.reply(msg.ID, nil, fsErr(errRefused, "notFound", "%s is not in the repository", p.Path))
			return
		}
		args = []string{"show", "--no-textconv", "HEAD:" + filepath.ToSlash(rel)}
	default:
		c.reply(msg.ID, nil, refusal(errParams, "op is status or original"))
		return
	}
	var sandboxed func(ctx context.Context, cwd, command string) *exec.Cmd
	if t, ok := s.parts.Loop.Tools.Get("bash"); ok {
		if b, ok := t.(tools.Bash); ok {
			sandboxed = gitSandbox(b)
		}
	}
	ctx, cancel := context.WithTimeout(c.root(), 20*time.Second)
	defer cancel()
	if sandboxed == nil {
		// No sandbox to hold a filter in (the none tier): git runs here only when the repository
		// configures no filter, diff or merge driver, which are what git would start.
		if drivers := gitDrivers(ctx, root); drivers != "" {
			c.reply(msg.ID, nil, refusal(errRefused, "this session has no sandbox, and the repository configures %s, which git would run; the SCM view does not run git here", drivers))
			return
		}
		sandboxed = func(ctx context.Context, cwd, line string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, "sh", "-c", line) // #nosec G204 -- gitLine quotes every word
			cmd.Dir = cwd
			return cmd
		}
	}
	cmd := sandboxed(ctx, root, gitLine(args...))
	if cmd.Err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "git %s: %v", p.Op, cmd.Err))
		return
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &fsCapped{b: &out, n: fsMaxRead}, &fsCapped{b: &errb, n: 4096}
	if err := cmd.Run(); err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "git %s: %s", p.Op, strings.TrimSpace(errb.String())))
		return
	}
	if p.Op == "original" {
		c.reply(msg.ID, map[string]any{"content": base64.StdEncoding.EncodeToString(out.Bytes())}, nil)
		return
	}
	changes := []any{}
	for _, entry := range strings.Split(out.String(), "\x00") {
		if len(entry) < 4 {
			continue
		}
		abs := filepath.Join(root, filepath.FromSlash(entry[3:]))
		if fsWithheld(s, abs) != "" {
			continue
		}
		changes = append(changes, map[string]any{"x": entry[:1], "y": entry[1:2], "path": abs})
	}
	c.reply(msg.ID, map[string]any{"root": root, "changes": changes}, nil)
}

// gitSandbox is what holds the SCM view's git: the bash tool's sandbox, or
// nil for none at all, as the none tier is (it wraps a command and holds
// nothing), so git then gets the no-sandbox check.
func gitSandbox(b tools.Bash) func(ctx context.Context, cwd, command string) *exec.Cmd {
	tier := b.Isolation.Tier
	if b.RanUnder != nil {
		if t := b.RanUnder(); t != "" {
			tier = t
		}
	}
	if tier == string(sandbox.TierNone) {
		return nil
	}
	return b.Sandbox
}

// gitDrivers names the filter, diff and merge drivers the repository's git
// configuration defines (any scope git reads), or "". Reading configuration
// runs nothing.
func gitDrivers(ctx context.Context, root string) string {
	line := gitLine("config", "--get-regexp", `^(filter|diff|merge)\..*\.(clean|smudge|process|command|textconv|driver)$`)
	cmd := exec.CommandContext(ctx, "sh", "-c", line) // #nosec G204 -- gitLine quotes every word
	cmd.Dir = root
	out, _ := cmd.Output()
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if k, _, _ := strings.Cut(l, " "); k != "" {
			names = append(names, k)
		}
	}
	if len(names) > 5 {
		names = append(names[:5], "…")
	}
	return strings.Join(names, ", ")
}

type fsCapped struct {
	b *bytes.Buffer
	n int
}

func (l *fsCapped) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}
