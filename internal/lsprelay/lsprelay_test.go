package lsprelay

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInside(t *testing.T) {
	roots := []string{"/w"}
	for body, want := range map[string]bool{
		`{"textDocument":{"uri":"file:///w/a.go"}}`:                          true,
		`{"textDocument":{"uri":"file:///w"}}`:                               true,
		`{"textDocument":{"uri":"file:///etc/passwd"}}`:                      false,
		`{"textDocument":{"uri":"file:///w/../etc/passwd"}}`:                 false,
		`{"textDocument":{"uri":"file://evil.host/w/a.go"}}`:                 false,
		`{"changes":{"file:///w/a.go":[]},"x":["FILE:///root/.ssh/id_rsa"]}`: true, // x is not a URI position
		`{"changes":{"file:///etc/passwd":[]}}`:                              false,
		`{"uri":"https://example.com"}`:                                      false, // not a file
		`{"text":"hello"}`:                                                   true,
		`{"text":"file:///etc/passwd is where it starts"}`:                   true, // document text, not a URI
		`not json`: false,
	} {
		if Inside([]byte(body), roots) != want {
			t.Errorf("%s: %v", body, !want)
		}
	}
}

func refusal(t *testing.T, err error) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || r.Reason == "" {
		t.Fatalf("not a refusal: %v", err)
	}
}

// The editor's requests pass, rebuilt; initialize, settings, an unsafe
// command, an outside file and an answer to nothing are refused.
func TestFromEditor(t *testing.T) {
	f := &Filter{Roots: []string{"/w"}, Language: "go"}
	out, err := f.FromEditor([]byte(`{"jsonrpc":"2.0","id":3,"method":"textDocument/hover","params":{"textDocument":{"uri":"file:///w/a.go"},"position":{"line":1,"character":2}}}`))
	if err != nil || !strings.Contains(string(out), `"textDocument/hover"`) {
		t.Fatalf("%s %v", out, err)
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"workspace/didChangeConfiguration","params":{"settings":{"gopls":{"env":{"GOFLAGS":"-toolexec=/bin/sh"}}}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"workspace/executeCommand","params":{"command":"gopls.run_go_work_command","arguments":[]}}`,
		`{"jsonrpc":"2.0","id":5,"method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///etc/passwd"}}}`,
		`{"jsonrpc":"2.0","id":6,"result":null}`,
		`{"jsonrpc":"2.0","id":7,"method":"x","extra":1}`,
	} {
		_, err := f.FromEditor([]byte(body))
		refusal(t, err)
	}
	if out, err := f.FromEditor([]byte(`{"jsonrpc":"2.0","id":8,"method":"workspace/executeCommand","params":{"command":"gopls.organize_imports","arguments":[]}}`)); err != nil || out == nil {
		t.Fatalf("a safe command was refused: %v", err)
	}
	// The editor answers what the server asked, and settings stay the defaults.
	if fwd, reply := f.FromServer([]byte(`{"jsonrpc":"2.0","id":"s1","method":"workspace/configuration","params":{"items":[{"section":"gopls"}]}}`)); fwd == nil || reply != nil {
		t.Fatal("the server's configuration request was not passed")
	}
	out, err = f.FromEditor([]byte(`{"jsonrpc":"2.0","id":"s1","result":[{"env":{"GOFLAGS":"-toolexec=x"}}]}`))
	if err != nil || !strings.Contains(string(out), `"result":[null]`) {
		t.Fatalf("configuration answer %s %v", out, err)
	}
	if _, err := f.FromEditor([]byte(`{"jsonrpc":"2.0","id":"s1","result":[null]}`)); err == nil {
		t.Fatal("a second answer to the same request was passed")
	}
	f.FromServer([]byte(`{"jsonrpc":"2.0","id":9,"method":"workspace/applyEdit","params":{"edit":{"changes":{"file:///w/a.go":[]}}}}`))
	if out, err := f.FromEditor([]byte(`{"jsonrpc":"2.0","id":9,"result":{"applied":true}}`)); err != nil || !strings.Contains(string(out), `"applied":true`) {
		t.Fatalf("applyEdit answer %s %v", out, err)
	}
}

// The server's request to open a page is answered by the engine; an outside
// file in a request is refused to it, in a notification dropped, and in an
// answer replaced by null.
func TestFromServer(t *testing.T) {
	f := &Filter{Roots: []string{"/w"}, Language: "go"}
	fwd, reply := f.FromServer([]byte(`{"jsonrpc":"2.0","id":1,"method":"window/showDocument","params":{"uri":"https://evil.example","external":true}}`))
	if fwd != nil || !strings.Contains(string(reply), `"success":false`) {
		t.Fatalf("showDocument: %s %s", fwd, reply)
	}
	fwd, reply = f.FromServer([]byte(`{"jsonrpc":"2.0","id":2,"method":"workspace/applyEdit","params":{"edit":{"changes":{"file:///etc/cron.d/x":[]}}}}`))
	if fwd != nil || !strings.Contains(string(reply), `"error"`) {
		t.Fatalf("outside applyEdit: %s %s", fwd, reply)
	}
	if fwd, reply = f.FromServer([]byte(`{"jsonrpc":"2.0","method":"textDocument/publishDiagnostics","params":{"uri":"file:///usr/lib/go/x.go","diagnostics":[]}}`)); fwd != nil || reply != nil {
		t.Fatalf("outside diagnostics: %s %s", fwd, reply)
	}
	fwd, _ = f.FromServer([]byte(`{"jsonrpc":"2.0","id":3,"result":[{"uri":"file:///usr/lib/go/src/fmt/print.go"}]}`))
	var m map[string]json.RawMessage
	if json.Unmarshal(fwd, &m) != nil || string(m["result"]) != "null" || string(m["id"]) != "3" {
		t.Fatalf("outside answer: %s", fwd)
	}
	if fwd, reply = f.FromServer([]byte(`{"jsonrpc":"2.0","id":4,"result":{"contents":"ok"}}`)); string(fwd) == "" || reply != nil {
		t.Fatal("an ordinary answer was not forwarded")
	}
}

func TestReadAndCommandLine(t *testing.T) {
	if _, err := Read(bufio.NewReader(strings.NewReader("Content-Length: 99999999999\r\n\r\n"))); err == nil {
		t.Fatal("a huge message was taken")
	}
	m, err := Read(bufio.NewReader(strings.NewReader("Content-Length: 2\r\n\r\n{}")))
	if err != nil || string(m) != "{}" {
		t.Fatalf("%q %v", m, err)
	}
	if got := CommandLine([]string{"basedpyright-langserver", "--stdio", "a b"}); got != `basedpyright-langserver --stdio 'a b'` {
		t.Fatal(got)
	}
}

// A key twice, or twice but for case, anywhere in a message, is refused one
// way and dropped the other: the filter and the reader must see one message.
func TestKeysThatRepeatButForCase(t *testing.T) {
	f := &Filter{Roots: []string{"/w"}, Language: "go"}
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"workspace/executeCommand","params":{"command":"gopls.organize_imports","Command":"gopls.run_go_work_command"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"textDocument/hover","Method":"workspace/executeCommand","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///w/a.go","URI":"file:///etc/passwd"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"x","params":{},"params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"x"} {"jsonrpc":"2.0"}`,
		// Unicode folds Go's encoding/json matches: ſ (U+017F) is s, K (U+212A) is k.
		`{"jsonrpc":"2.0","id":1,"method":"textDocument/codeAction","params":{"context":{"only":["quickfix"],"\u212aind":"x","kind":"y"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"textDocument/codeAction","params":{"context":{"kind":"y","\u212aind":"x"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"workspace/executeCommand","params":{"command":"gopls.organize_imports","argument\u017f":[1],"arguments":[]}}`,
		`{"jsonrpc":"2.0","id":1,"method":"workspace/executeCommand","params":{"arguments":[],"argumentſ":[1],"command":"gopls.organize_imports"}}`,
	} {
		_, err := f.FromEditor([]byte(body))
		refusal(t, err)
	}
	if fwd, reply := f.FromServer([]byte(`{"jsonrpc":"2.0","id":1,"method":"window/showDocument","params":{"uri":"file:///w/a.go","URI":"https://evil.example"}}`)); fwd != nil || reply != nil {
		t.Fatalf("a server message with a case-twin key was passed: %s %s", fwd, reply)
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":2,"method":"workspace/applyEdit","params":{"edit":{"changes":{}},"edits":0,"edit\u017f":1,"label":"x"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"window/showMessage","params":{"\u212aind":1,"kind":2,"message":"x"}}`,
	} {
		if fwd, reply := f.FromServer([]byte(body)); fwd != nil {
			t.Fatalf("a server message with a folded twin key was passed: %s %s", fwd, reply)
		}
	}
}

// URI positions: a schemeless, relative or percent-encoded path, a URI of
// another scheme, one in a root's .abhed, and one through a link that
// leads out are all outside, both ways and in every file operation.
func TestURIPositions(t *testing.T) {
	ws := t.TempDir()
	out := t.TempDir()
	if err := os.Symlink(out, filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &Filter{Roots: []string{ws}, Language: "typescript"}
	in := URI(ws)
	for _, uri := range []string{"/etc/passwd", "%2Fetc%2Fpasswd", "etc/passwd", "file:etc/passwd", "untitled:x",
		in + "/.abhed/state.json", in + "/.ABHED/x", in + "/sub/../.abhed/x", in + "/link/secret", in + "/link/new/file"} {
		if _, err := f.FromEditor([]byte(`{"jsonrpc":"2.0","id":1,"method":"textDocument/definition","params":{"textDocument":{"uri":"` + uri + `"},"position":{"line":0,"character":0}}}`)); err == nil {
			t.Errorf("Studio's %s was passed", uri)
		}
	}
	for _, edit := range []string{
		`{"documentChanges":[{"kind":"create","uri":"/etc/x"}]}`,
		`{"documentChanges":[{"kind":"rename","oldUri":"` + in + `/sub/a.ts","newUri":"/Users/x/.zshrc"}]}`,
		`{"documentChanges":[{"kind":"rename","oldUri":"` + in + `/sub/a.ts","newUri":"` + in + `/.abhed/a.ts"}]}`,
		`{"documentChanges":[{"kind":"delete","uri":"` + in + `/.abhed/state.json"}]}`,
		`{"documentChanges":[{"kind":"create","uri":"` + in + `/link/x.ts"}]}`,
		`{"changes":{"%2Fetc%2Fpasswd":[]}}`,
	} {
		fwd, reply := f.FromServer([]byte(`{"jsonrpc":"2.0","id":7,"method":"workspace/applyEdit","params":{"edit":` + edit + `}}`))
		if fwd != nil || !strings.Contains(string(reply), `"error"`) {
			t.Errorf("applyEdit %s: forwarded %s", edit, fwd)
		}
	}
	ok := `{"documentChanges":[{"kind":"create","uri":"` + in + `/sub/new.ts"},{"kind":"rename","oldUri":"` + in + `/sub/new.ts","newUri":"` + in + `/sub/b.ts"}]}`
	if fwd, _ := f.FromServer([]byte(`{"jsonrpc":"2.0","id":8,"method":"workspace/applyEdit","params":{"edit":` + ok + `}}`)); fwd == nil {
		t.Fatal("an edit inside the workspace was refused")
	}
}

// Only standard methods, and a server's reviewed own ones, pass from Studio.
func TestEditorMethodsAreAnAllowList(t *testing.T) {
	f := &Filter{Roots: []string{"/w"}, Language: "rust"}
	for _, m := range []string{"experimental/runTest", "rust-analyzer/runFlycheck", "rust-analyzer/rebuildProcMacros",
		"rust-analyzer/reloadWorkspace", "gopls/whatever", "textDocument/madeUp"} {
		if _, err := f.FromEditor([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + m + `","params":{}}`)); err == nil {
			t.Errorf("%s was passed", m)
		}
	}
	for _, m := range []string{"textDocument/completion", "workspace/symbol", "$/cancelRequest", "shutdown"} {
		if _, err := f.FromEditor([]byte(`{"jsonrpc":"2.0","id":2,"method":"` + m + `","params":{}}`)); err != nil {
			t.Errorf("%s was refused: %v", m, err)
		}
	}
	f.Methods = []string{"rust-analyzer/analyzerStatus"}
	if _, err := f.FromEditor([]byte(`{"jsonrpc":"2.0","id":3,"method":"rust-analyzer/analyzerStatus","params":{}}`)); err != nil {
		t.Fatalf("a reviewed server method was refused: %v", err)
	}
}

// The server's settings are the engine's: rust-analyzer's build scripts,
// proc macros and check on save are off whatever Studio answers.
func TestConfigurationIsTheEngines(t *testing.T) {
	opts, sections := SafeSettings("rust", "")
	f := &Filter{Roots: []string{"/w"}, Language: "rust", Settings: sections}
	if !strings.Contains(string(opts), `"buildScripts":{"enable":false}`) || !strings.Contains(string(opts), `"procMacro":{"enable":false}`) || !strings.Contains(string(opts), `"checkOnSave":false`) {
		t.Fatalf("options %s", opts)
	}
	f.FromServer([]byte(`{"jsonrpc":"2.0","id":"c1","method":"workspace/configuration","params":{"items":[{"section":"rust-analyzer"},{"section":"other"}]}}`))
	out, err := f.FromEditor([]byte(`{"jsonrpc":"2.0","id":"c1","result":[{"procMacro":{"enable":true}},{"x":1}]}`))
	if err != nil || !strings.Contains(string(out), `"procMacro":{"enable":false}`) || !strings.Contains(string(out), `,null]`) {
		t.Fatalf("answer %s %v", out, err)
	}
	if o, _ := SafeSettings("typescript", "/opt/ts/lib/tsserver.js"); !strings.Contains(string(o), `"tsserver":{"path":"/opt/ts/lib/tsserver.js"}`) {
		t.Fatalf("typescript options %s", o)
	}
	if p := InitParams("/w", nil, opts); !strings.Contains(string(p), `"initializationOptions":{"cargo"`) {
		t.Fatalf("init params %s", p)
	}
}

// A request id still waiting is not taken again: Studio's answer to the
// first cannot reach the second.
func TestPendingRequestIDIsNotReused(t *testing.T) {
	f := &Filter{Roots: []string{"/w"}, Language: "go"}
	if fwd, _ := f.FromServer([]byte(`{"jsonrpc":"2.0","id":5,"method":"workspace/configuration","params":{"items":[{"section":"gopls"}]}}`)); fwd == nil {
		t.Fatal("the first request was not passed")
	}
	fwd, reply := f.FromServer([]byte(`{"jsonrpc":"2.0","id":5,"method":"workspace/applyEdit","params":{"edit":{}}}`))
	if fwd != nil || !strings.Contains(string(reply), "still waiting") {
		t.Fatalf("a reused id: %s %s", fwd, reply)
	}
	out, _ := f.FromEditor([]byte(`{"jsonrpc":"2.0","id":5,"result":[{"build":{"env":{"GOFLAGS":"-toolexec=x"}}}]}`))
	if strings.Contains(string(out), "toolexec") {
		t.Fatalf("Studio's settings reached the server: %s", out)
	}
}

// A message nested past the bound, under the size limit, is refused without
// a stack overflow, from the server and from Studio; ſ twins are refused.
func TestDeepAndFoldedMessages(t *testing.T) {
	f := &Filter{Roots: []string{"/w"}, Language: "go"}
	deep := `{"jsonrpc":"2.0","method":"x","params":` + strings.Repeat("[", 4<<20-64) + strings.Repeat("]", 4<<20-64) + `}`
	if len(deep) > MaxMessage {
		t.Fatalf("%d bytes", len(deep))
	}
	if fwd, reply := f.FromServer([]byte(deep)); fwd != nil || reply != nil {
		t.Fatal("a deep message was passed")
	}
	if _, err := f.FromEditor([]byte(deep)); err == nil {
		t.Fatal("a deep message from Studio was passed")
	}
	if UniqueKeys([]byte(`{"a":`+strings.Repeat("[", MaxDepth+5)+strings.Repeat("]", MaxDepth+5)+`}`)) == nil {
		t.Fatal("past MaxDepth passed")
	}
	if _, err := f.FromEditor([]byte(`{"jsonrpc":"2.0","id":1,"method":"textDocument/hover","params":{"textDocument":{"uri":"file:///w/a.go"},"textDocument\u017f":{"uri":"file:///etc/passwd"}}}`)); err == nil {
		t.Fatal("a folded twin key was passed")
	}
}

// The script the server runs under works through bash -c and sh -c: the
// variables reach the server, a missing server exits ExitNoServer, and
// typescript-language-server gets the TypeScript beside it named.
func TestScriptRunsUnderTheShells(t *testing.T) {
	bin := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("gopls", `echo "argv=$* GOTOOLCHAIN=$GOTOOLCHAIN GOFLAGS=[$GOFLAGS] GOENV=$GOENV"`)
	write("typescript-language-server", `echo "ts argv=$*"`)
	write("node", `printf /opt/ts/lib/tsserver.js`)
	for _, shell := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(shell); err != nil {
			continue
		}
		run := func(script string) (string, string, int) {
			cmd := exec.Command(shell, "-c", script)
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "GOFLAGS=-toolexec=/evil", "GOENV=/evil"}
			var so, se strings.Builder
			cmd.Stdout, cmd.Stderr = &so, &se
			err := cmd.Run()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
			return so.String(), se.String(), code
		}
		out, errOut, code := run(Script([]string{"gopls"}))
		if code != 0 || !strings.Contains(out, "GOTOOLCHAIN=local GOFLAGS=[] GOENV=off") {
			t.Fatalf("%s gopls: %d %q %q", shell, code, out, errOut)
		}
		out, errOut, code = run(Script([]string{"typescript-language-server", "--stdio"}))
		if code != 0 || !strings.Contains(out, "ts argv=--stdio") || !strings.Contains(errOut, TSServerMarker+"/opt/ts/lib/tsserver.js") {
			t.Fatalf("%s typescript: %d %q %q", shell, code, out, errOut)
		}
		if _, _, code = run(Script([]string{"rust-analyzer"})); code != ExitNoServer {
			t.Fatalf("%s missing server exited %d", shell, code)
		}
	}
	write("node", `exit 0`)
	cmd := exec.Command("/bin/sh", "-c", Script([]string{"typescript-language-server", "--stdio"}))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != ExitNoTypeScript {
		t.Fatalf("no TypeScript beside the server: %v", err)
	}
}
