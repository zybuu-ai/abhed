// Package lsprelay is what the workbench and Abhed Studio share to run a
// language server inside a session's sandbox: the framing, read with the
// server taken as untrusted; the initialize params the server is given; the
// command line policy judges; and the filter on messages each way, which
// keeps file URIs inside the session's roots, refuses commands that could
// have the server run programs, and drops requests to open anything that is
// not a file.
package lsprelay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/foldkey"
)

// Bounds on what a server or an editor may send.
const (
	MaxMessage = 8 << 20
	// MaxDepth bounds how deeply a message nests: far past any real LSP
	// message (a document symbol tree or selection range chain), far short
	// of a stack a deep message could exhaust.
	MaxDepth   = 256
	maxHeader  = 1 << 10
	maxHeaders = 16
)

// Env is set for every language server, as assignments in front of its
// command: a go.mod's toolchain line cannot have the go command fetch and
// run another Go, and neither GOFLAGS nor a go env file (GOENV) can add
// flags such as -toolexec to the go commands gopls runs.
const Env = "GOTOOLCHAIN=local GOFLAGS= GOENV=off "

// exitNoServer and exitNoTypeScript are the codes Script exits with when
// the server, or a TypeScript of its own, is missing.
const (
	ExitNoServer     = 96
	ExitNoTypeScript = 95
)

// TSServerMarker starts the line Script writes to stderr naming the
// tsserver.js beside typescript-language-server, which the engine gives it
// as tsserver.path so it never runs the workspace's own TypeScript.
const TSServerMarker = "abhed-lsp-tsserver:"

// Script is the shell the server runs under in the sandbox: it fails with
// ExitNoServer when argv[0] is not on PATH, and execs the server with Env
// through env, since after exec an assignment would be taken as the
// program. For typescript-language-server it first finds the TypeScript
// installed with it (never one under the workspace) and names it on stderr.
func Script(argv []string) string {
	name := argv[0]
	script := "command -v " + CommandLine(argv[:1]) + " >/dev/null 2>&1 || { echo 'abhed-lsp: " + strings.ReplaceAll(name, "'", "") +
		" is not on PATH' >&2; exit " + strconv.Itoa(ExitNoServer) + "; }; "
	if filepath.Base(name) == "typescript-language-server" {
		script += `ts=$(node -e 'const p=require("path"),f=require("fs");try{process.stdout.write(require.resolve("typescript/lib/tsserver.js",{paths:[p.dirname(f.realpathSync(process.argv[1]))]}))}catch(e){}' "$(command -v ` + CommandLine(argv[:1]) + `)" 2>/dev/null); ` +
			`[ -n "$ts" ] || { echo 'abhed-lsp: no TypeScript is installed beside typescript-language-server, and the workspace'"'"'s own is not used' >&2; exit ` + strconv.Itoa(ExitNoTypeScript) + "; }; " +
			`echo "` + TSServerMarker + `$ts" >&2; `
	}
	return script + "exec env " + Env + CommandLine(argv)
}

// Read reads one Content-Length framed message, its headers bounded in
// length and number.
func Read(br *bufio.Reader) ([]byte, error) {
	size := -1
	for headers := 0; ; headers++ {
		raw, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(raw) > maxHeader || headers >= maxHeaders {
			return nil, errors.New("language server header too long")
		}
		if err != nil {
			return nil, err
		}
		line := strings.TrimRight(string(raw), "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 || n > MaxMessage {
				return nil, errors.New("bad Content-Length")
			}
			size = n
		}
	}
	if size < 0 {
		return nil, errors.New("no Content-Length")
	}
	msg := make([]byte, size)
	if _, err := io.ReadFull(br, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// Write frames one message.
func Write(w io.Writer, body []byte) error {
	_, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

var safeWord = regexp.MustCompile(`^[A-Za-z0-9_./=:@%+,-]+$`)

// CommandLine is argv as the policy reads a bash command.
func CommandLine(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		if safeWord.MatchString(a) {
			words[i] = a
		} else {
			words[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(words, " ")
}

// URI is root's file URI, escaped as an editor's encodeURIComponent does.
func URI(root string) string {
	var b strings.Builder
	b.WriteString("file://")
	for i := 0; i < len(root); i++ {
		c := root[i]
		if c == '/' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// InitParams are the initialize params a server gets for the workspace at
// root with the editor's capabilities: the engine's own
// initializationOptions (SafeSettings), never the editor's, which can set a
// server's environment or tools, and no process id.
func InitParams(root string, capabilities, options json.RawMessage) json.RawMessage {
	uri := URI(root)
	name := root[strings.LastIndex(root, "/")+1:]
	if len(capabilities) == 0 {
		capabilities = json.RawMessage(`{}`)
	}
	params := map[string]any{
		"processId": nil, "rootUri": uri,
		"workspaceFolders": []map[string]string{{"uri": uri, "name": name}},
		"capabilities":     capabilities,
	}
	if len(options) > 0 && string(options) != "null" {
		params["initializationOptions"] = options
	}
	b, _ := json.Marshal(params)
	return b
}

// SafeSettings are what the engine tells a known server, both as its
// initializationOptions and as its answer to workspace/configuration for
// the sections named: what turns off its running of workspace code where a
// setting can. tsserver is the tsserver.js beside typescript-language-server.
//
//   - rust-analyzer: build scripts, proc macros, check on save and cargo's
//     autoreload off, so it runs no build.rs, no proc macro and no cargo check.
//   - typescript-language-server: tsserver.path pinned to its own TypeScript,
//     never the workspace's node_modules.
//   - gopls and basedpyright: their defaults (null); gopls's go commands get
//     Env, which clears GOFLAGS and go env files.
func SafeSettings(language, tsserver string) (options json.RawMessage, sections map[string]json.RawMessage) {
	switch language {
	case "rust":
		ra := json.RawMessage(`{"cargo":{"buildScripts":{"enable":false},"autoreload":false},"procMacro":{"enable":false},"checkOnSave":false}`)
		return ra, map[string]json.RawMessage{"rust-analyzer": ra}
	case "typescript", "javascript":
		if tsserver == "" {
			return nil, nil
		}
		b, _ := json.Marshal(map[string]any{"tsserver": map[string]string{"path": tsserver}})
		return b, nil
	}
	return nil, nil
}

// Nulls reports whether an answer is null or a list of nulls.
func Nulls(raw json.RawMessage) bool {
	if raw == nil {
		return false
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		for _, v := range list {
			if string(v) != "null" {
				return false
			}
		}
		return true
	}
	return string(bytes.TrimSpace(raw)) == "null"
}

// uriKeys are the keys whose string value is a URI in the LSP: a
// document, a location, a file operation, a folder. Only these are checked:
// text that happens to start with "file:" is a document's content.
var uriKeys = map[string]bool{"uri": true, "oldUri": true, "newUri": true, "targetUri": true,
	"rootUri": true, "scopeUri": true, "baseUri": true}

// Inside reports whether every URI in a URI position of raw (uriKeys, the
// keys of a WorkspaceEdit's changes, and rootPath) names a file under one
// of roots. A URI there that is not an absolute file URI is outside, as is
// one under a root's .abhed folder, or one whose real path, links followed,
// is outside the roots' real paths. Other strings are not looked at.
func Inside(raw []byte, roots []string) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	rs := resolveRoots(roots)
	return walkURIs(v, func(u string) bool { return fileInside(u, rs) }, func(p string) bool { return pathInside(p, rs) })
}

func walkURIs(v any, uri, path func(string) bool) bool {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if !walkURIs(e, uri, path) {
				return false
			}
		}
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok {
				switch {
				case uriKeys[k] && !uri(s):
					return false
				case k == "rootPath" && !path(s):
					return false
				}
				continue
			}
			// A WorkspaceEdit's changes name their files as keys.
			if c, ok := e.(map[string]any); ok && k == "changes" {
				for f := range c {
					if !uri(f) {
						return false
					}
				}
			}
			if !walkURIs(e, uri, path) {
				return false
			}
		}
	}
	return true
}

// root is a session folder as given and with its links resolved.
type root struct{ given, real string }

func resolveRoots(roots []string) []root {
	out := make([]root, 0, len(roots))
	for _, r := range roots {
		c := filepath.Clean(r)
		// Resolved as a path in it is, so a root that does not exist here
		// (a container's) is compared alike.
		out = append(out, root{c, realPath(c)})
	}
	return out
}

// fileInside reports whether u is an absolute file URI, on no other host,
// whose path is under one of roots.
func fileInside(u string, roots []root) bool {
	p, err := url.Parse(u)
	if err != nil || !strings.EqualFold(p.Scheme, "file") || p.Opaque != "" || (p.Host != "" && p.Host != "localhost") {
		return false
	}
	return pathInside(p.Path, roots)
}

// pathInside reports whether the absolute path p (already percent-decoded)
// is under a root, not in its .abhed folder, and still under it with links
// followed.
func pathInside(p string, roots []root) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	p = filepath.Clean(p)
	real := realPath(p)
	for _, r := range roots {
		if under(r.given, p) && under(r.real, real) {
			return true
		}
	}
	return false
}

// under reports whether p is base or below it, and not in base's .abhed.
func under(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel != "." && !filepath.IsLocal(rel) {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.EqualFold(seg, ".abhed") {
			return false
		}
	}
	return true
}

// realPath is p with the links in its deepest existing ancestor followed.
func realPath(p string) string {
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		up := filepath.Dir(cur)
		if up == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = up
	}
}

// SafeCommands are the workspace/executeCommand commands each known server
// runs on its own documents without starting other programs. A command not
// listed is refused.
var SafeCommands = map[string][]string{
	"go": {"gopls.add_import", "gopls.apply_fix", "gopls.change_signature", "gopls.extract_to_new_file",
		"gopls.fill_struct", "gopls.list_imports", "gopls.list_known_packages", "gopls.organize_imports",
		"gopls.remove_dependency", "gopls.add_test", "gopls.doc"},
	"python":     {"basedpyright.organizeimports", "basedpyright.addoptionalforparam", "pyright.organizeimports"},
	"typescript": {"_typescript.organizeImports", "_typescript.applyRenameFile", "_typescript.applyCompletionCodeAction", "_typescript.applyWorkspaceEdit"},
	"javascript": {"_typescript.organizeImports", "_typescript.applyRenameFile", "_typescript.applyCompletionCodeAction", "_typescript.applyWorkspaceEdit"},
	"rust":       {"rust-analyzer.applySnippetWorkspaceEdit"},
}

// Filter carries one server's messages between it and an editor.
type Filter struct {
	// Roots are the session's folders; a file URI elsewhere is refused.
	Roots []string
	// Language picks the commands in SafeCommands.
	Language string

	// Settings are the engine's answer to workspace/configuration, by
	// section (SafeSettings); a section not here is answered null.
	Settings map[string]json.RawMessage
	// Methods are the server's own requests and notifications an editor may
	// send besides the standard ones (EditorMethods).
	Methods []string

	mu sync.Mutex
	// asked are the server's requests waiting for the editor, by id: only
	// those may be answered, and how depends on the request.
	asked map[string]pending
}

// pending is one of the server's requests waiting for the editor.
type pending struct {
	method string
	params json.RawMessage
}

// Refusal is an editor message the filter did not pass, with why.
type Refusal struct {
	ID     json.RawMessage
	Reason string
}

func (r *Refusal) Error() string { return r.Reason }

// message is the part of a JSON-RPC message the filter reads.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// strictMessage reads a message by its exact keys. One with a key twice,
// or two keys that differ only in case, anywhere in it, is refused: Go's
// decoding of a struct matches keys without case and keeps the last, while
// a server or an editor reads the exact key, so the filter and the reader
// could see different messages.
func strictMessage(body []byte) (message, error) {
	if err := UniqueKeys(body); err != nil {
		return message{}, err
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(body, &keys) != nil {
		return message{}, errors.New("not a JSON-RPC message (a batch, or not an object)")
	}
	var m message
	for k, v := range keys {
		switch k {
		case "jsonrpc":
			if json.Unmarshal(v, &m.JSONRPC) != nil {
				return message{}, errors.New("jsonrpc is not a string")
			}
		case "id":
			m.ID = v
		case "method":
			if json.Unmarshal(v, &m.Method) != nil {
				return message{}, errors.New("method is not a string")
			}
		case "params":
			m.Params = v
		case "result":
			m.Result = v
		case "error":
			m.Error = v
		default:
			return message{}, fmt.Errorf("a JSON-RPC message has no %q", k)
		}
	}
	return m, nil
}

// field is one key of a JSON object, read by its exact name.
func field(obj json.RawMessage, key string) json.RawMessage {
	var keys map[string]json.RawMessage
	if json.Unmarshal(obj, &keys) != nil {
		return nil
	}
	return keys[key]
}

// stringField is field read as a string; "" when it is absent or not one.
func stringField(obj json.RawMessage, key string) string {
	var v string
	_ = json.Unmarshal(field(obj, key), &v)
	return v
}

// UniqueKeys refuses JSON with an object whose keys repeat, exactly or but
// for case under Unicode simple folding (as Go's encoding/json matches
// them), at any depth.
func UniqueKeys(body []byte) error {
	// Valid first: it bounds nesting itself, and walk below never sees what
	// it refuses. walk bounds its own depth too, so its recursion cannot
	// overflow the stack whatever reaches it.
	if !json.Valid(body) {
		return errors.New("not a JSON-RPC message: not valid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	depth := 0
	var walk func() error
	walk = func() error {
		if depth++; depth > MaxDepth {
			return fmt.Errorf("nested deeper than %d", MaxDepth)
		}
		defer func() { depth-- }()
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k, _ := kt.(string)
				// As encoding/json and strings.EqualFold compare: ſ is s, K is k.
				fold := foldkey.Key(k)
				if seen[fold] {
					return fmt.Errorf("the key %q appears twice, or twice but for case", k)
				}
				seen[fold] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case json.Delim('['):
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return fmt.Errorf("not a JSON-RPC message: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("not a JSON-RPC message: more than one value")
	}
	return nil
}

// EditorMethods are the standard LSP 3.17 methods an editor may send a
// server. Anything else, a server's own extension such as rust-analyzer's
// experimental/runTest or rust-analyzer/runFlycheck, is refused unless it is
// in the Filter's Methods. initialize and initialized are the engine's, and
// workspace/didChangeConfiguration is refused: settings can set a server's
// environment or tools.
var EditorMethods = map[string]bool{
	"shutdown": true, "exit": true, "$/cancelRequest": true, "$/setTrace": true, "$/progress": true,
	"window/workDoneProgress/cancel": true,
	"textDocument/didOpen":           true, "textDocument/didChange": true, "textDocument/didClose": true,
	"textDocument/didSave": true, "textDocument/willSave": true, "textDocument/willSaveWaitUntil": true,
	"textDocument/completion": true, "completionItem/resolve": true, "textDocument/hover": true,
	"textDocument/signatureHelp": true, "textDocument/declaration": true, "textDocument/definition": true,
	"textDocument/typeDefinition": true, "textDocument/implementation": true, "textDocument/references": true,
	"textDocument/documentHighlight": true, "textDocument/documentSymbol": true,
	"textDocument/codeAction": true, "codeAction/resolve": true, "textDocument/codeLens": true, "codeLens/resolve": true,
	"textDocument/documentLink": true, "documentLink/resolve": true, "textDocument/documentColor": true,
	"textDocument/colorPresentation": true, "textDocument/formatting": true, "textDocument/rangeFormatting": true,
	"textDocument/onTypeFormatting": true, "textDocument/rename": true, "textDocument/prepareRename": true,
	"textDocument/foldingRange": true, "textDocument/selectionRange": true,
	"textDocument/semanticTokens/full": true, "textDocument/semanticTokens/full/delta": true, "textDocument/semanticTokens/range": true,
	"textDocument/linkedEditingRange": true, "textDocument/moniker": true, "textDocument/inlayHint": true, "inlayHint/resolve": true,
	"textDocument/inlineValue": true, "textDocument/diagnostic": true, "workspace/diagnostic": true,
	"textDocument/prepareCallHierarchy": true, "callHierarchy/incomingCalls": true, "callHierarchy/outgoingCalls": true,
	"textDocument/prepareTypeHierarchy": true, "typeHierarchy/supertypes": true, "typeHierarchy/subtypes": true,
	"workspace/symbol": true, "workspaceSymbol/resolve": true, "workspace/executeCommand": true,
	"workspace/didChangeWorkspaceFolders": true, "workspace/didChangeWatchedFiles": true,
	"workspace/willCreateFiles": true, "workspace/didCreateFiles": true, "workspace/willRenameFiles": true,
	"workspace/didRenameFiles": true, "workspace/willDeleteFiles": true, "workspace/didDeleteFiles": true,
}

// ServerMethods are each known server's own methods reviewed as running no
// program: none yet. rust-analyzer's experimental/runTest,
// rust-analyzer/runFlycheck, rebuildProcMacros and reloadWorkspace run
// cargo, build scripts or tests, so they stay refused.
var ServerMethods = map[string][]string{}

// FromEditor returns the editor's message rebuilt for the server, or a
// Refusal: a method the engine keeps, a command not in SafeCommands, an
// answer to nothing the server asked, or a file URI outside the roots.
func (f *Filter) FromEditor(body []byte) ([]byte, error) {
	if len(body) > MaxMessage {
		return nil, &Refusal{Reason: "the message is too large"}
	}
	m, err := strictMessage(body)
	if err != nil {
		return nil, &Refusal{Reason: err.Error()}
	}
	if !Inside(body, f.Roots) {
		return nil, &Refusal{ID: m.ID, Reason: "a file outside the session's workspace"}
	}
	if m.Method != "" {
		switch {
		case m.Method == "initialize" || m.Method == "initialized":
			return nil, &Refusal{ID: m.ID, Reason: "the engine has already initialized the server"}
		case m.Method == "workspace/didChangeConfiguration":
			return nil, &Refusal{ID: m.ID, Reason: "settings can set a server's environment or tools; it keeps the engine's"}
		case !EditorMethods[m.Method] && !slices.Contains(f.Methods, m.Method):
			return nil, &Refusal{ID: m.ID, Reason: fmt.Sprintf("%q is not a standard LSP method, and not one reviewed for this server; a server's own methods can have it run programs", m.Method)}
		}
		if m.Method == "workspace/executeCommand" {
			command := stringField(m.Params, "command")
			if !slices.Contains(SafeCommands[f.Language], command) {
				return nil, &Refusal{ID: m.ID, Reason: fmt.Sprintf("the command %q could have the server run programs, so it is not sent", command)}
			}
		}
		return rebuild(m)
	}
	id := string(m.ID)
	f.mu.Lock()
	req, asked := f.asked[id]
	delete(f.asked, id)
	f.mu.Unlock()
	if !asked {
		return nil, &Refusal{Reason: "an answer to nothing the server asked"}
	}
	// The server's settings are the engine's, whatever the editor answers:
	// one such as env or build flags can have it run programs.
	if req.method == "workspace/configuration" {
		m.Result, m.Error = f.configuration(req.params), nil
	}
	return rebuild(message{JSONRPC: "2.0", ID: m.ID, Result: m.Result, Error: m.Error})
}

// configuration is the engine's answer to a workspace/configuration: per
// item, the Settings for its section, else null.
func (f *Filter) configuration(params json.RawMessage) json.RawMessage {
	var p struct {
		Items []struct {
			Section string `json:"section"`
		} `json:"items"`
	}
	_ = json.Unmarshal(params, &p)
	out := make([]json.RawMessage, len(p.Items))
	for i, it := range p.Items {
		out[i] = json.RawMessage(`null`)
		if v, ok := f.Settings[it.Section]; ok {
			out[i] = v
		}
	}
	b, _ := json.Marshal(out)
	return b
}

func rebuild(m message) ([]byte, error) {
	m.JSONRPC = "2.0"
	if m.Result == nil && m.Error == nil && m.Method == "" {
		m.Result = json.RawMessage(`null`)
	}
	b, err := json.Marshal(m)
	return b, err
}

// FromServer decides what becomes of one server message: forward it to the
// editor (possibly rebuilt), or answer the server itself (reply non-nil)
// and send the editor nothing.
func (f *Filter) FromServer(body []byte) (forward, reply []byte) {
	m, err := strictMessage(body)
	if err != nil {
		return nil, nil
	}
	inside := Inside(body, f.Roots)
	switch {
	case m.Method != "" && len(m.ID) > 0: // a request of the server's
		if m.Method == "window/showDocument" {
			if uri := stringField(m.Params, "uri"); !strings.HasPrefix(strings.ToLower(uri), "file:") || !inside {
				// An external page or file is not opened for it.
				b, _ := json.Marshal(message{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage(`{"success":false}`)})
				return nil, b
			}
		}
		if !inside {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID,
				"error": map[string]any{"code": -32600, "message": "a file outside the session's workspace"}})
			return nil, b
		}
		f.mu.Lock()
		if f.asked == nil {
			f.asked = map[string]pending{}
		}
		_, dup := f.asked[string(m.ID)]
		full := len(f.asked) >= 1024
		if !dup && !full {
			f.asked[string(m.ID)] = pending{method: m.Method, params: m.Params}
		}
		f.mu.Unlock()
		if dup || full {
			// An id still waiting for an answer is not reused: the editor's
			// answer to one request must not be taken as the other's.
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID,
				"error": map[string]any{"code": -32600, "message": "a request id still waiting for an answer, or too many waiting"}})
			return nil, b
		}
		return body, nil
	case m.Method != "": // a notification
		if !inside {
			return nil, nil
		}
		return body, nil
	default: // an answer to the editor's request
		if !inside {
			b, _ := json.Marshal(message{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage(`null`)})
			return b, nil
		}
		return body, nil
	}
}
