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
	maxHeader  = 1 << 10
	maxHeaders = 16
)

// Env is set for every language server: a go.mod's toolchain line cannot
// have the go command fetch and run another Go.
const Env = "GOTOOLCHAIN=local "

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
// root with the editor's capabilities: no initializationOptions, which can
// set a server's environment or tools, and no process id.
func InitParams(root string, capabilities json.RawMessage) json.RawMessage {
	uri := URI(root)
	name := root[strings.LastIndex(root, "/")+1:]
	if len(capabilities) == 0 {
		capabilities = json.RawMessage(`{}`)
	}
	b, _ := json.Marshal(map[string]any{
		"processId": nil, "rootUri": uri,
		"workspaceFolders": []map[string]string{{"uri": uri, "name": name}},
		"capabilities":     capabilities,
	})
	return b
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

// Inside reports whether every file URI in raw names a path under one of
// roots; a URI of another scheme passes, and so does text with none.
func Inside(raw []byte, roots []string) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	return walkURIs(v, func(u string) bool { return fileInside(u, roots) })
}

func walkURIs(v any, ok func(string) bool) bool {
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(strings.ToLower(x), "file:") {
			return ok(x)
		}
	case []any:
		for _, e := range x {
			if !walkURIs(e, ok) {
				return false
			}
		}
	case map[string]any:
		// A WorkspaceEdit's changes name their files as keys.
		for k, e := range x {
			if !walkURIs(k, ok) || !walkURIs(e, ok) {
				return false
			}
		}
	}
	return true
}

// fileInside reports whether a file URI names a path under one of roots.
func fileInside(u string, roots []string) bool {
	p, err := url.Parse(u)
	if err != nil || !strings.EqualFold(p.Scheme, "file") || (p.Host != "" && p.Host != "localhost") {
		return false
	}
	path := filepath.Clean(p.Path)
	for _, r := range roots {
		rel, err := filepath.Rel(filepath.Clean(r), path)
		if err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return true
		}
	}
	return false
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

	mu sync.Mutex
	// asked are the server's requests waiting for the editor, by id, with
	// their method: only those may be answered, and how depends on it.
	asked map[string]string
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
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var walk func() error
	walk = func() error {
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

// refusedFromEditor are methods an editor may not send: the engine has
// already initialized the server, and settings can set its environment.
var refusedFromEditor = map[string]string{
	"initialize":                       "the engine has already initialized the server",
	"initialized":                      "the engine has already initialized the server",
	"workspace/didChangeConfiguration": "settings can set a server's environment or tools; it keeps its defaults",
}

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
		if why, ok := refusedFromEditor[m.Method]; ok {
			return nil, &Refusal{ID: m.ID, Reason: why}
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
	method, asked := f.asked[id]
	delete(f.asked, id)
	f.mu.Unlock()
	if !asked {
		return nil, &Refusal{Reason: "an answer to nothing the server asked"}
	}
	// Settings the server asks for stay its defaults: one such as env or
	// build flags can have it run programs, so only nulls are passed.
	if method == "workspace/configuration" && (m.Error != nil || !Nulls(m.Result)) {
		m.Result, m.Error = configurationNulls(m.Result), nil
	}
	return rebuild(message{JSONRPC: "2.0", ID: m.ID, Result: m.Result, Error: m.Error})
}

func configurationNulls(result json.RawMessage) json.RawMessage {
	var list []json.RawMessage
	if json.Unmarshal(result, &list) != nil {
		return json.RawMessage(`null`)
	}
	out := make([]string, len(list))
	for i := range out {
		out[i] = "null"
	}
	return json.RawMessage("[" + strings.Join(out, ",") + "]")
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
			f.asked = map[string]string{}
		}
		if len(f.asked) < 1024 {
			f.asked[string(m.ID)] = m.Method
		}
		f.mu.Unlock()
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
