package lsprelay

import (
	"bufio"
	"encoding/json"
	"errors"
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
		`{"changes":{"file:///w/a.go":[]},"x":["FILE:///root/.ssh/id_rsa"]}`: false,
		`{"uri":"https://example.com"}`:                                      true,
		`{"text":"hello"}`:                                                   true,
		`not json`:                                                           false,
	} {
		if Inside([]byte(body), roots) != want {
			t.Errorf("%s: %v", body, !want)
		}
	}
}

func refusal(t *testing.T, err error) *Refusal {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("not a refusal: %v", err)
	}
	return r
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
	} {
		_, err := f.FromEditor([]byte(body))
		refusal(t, err)
	}
	if fwd, reply := f.FromServer([]byte(`{"jsonrpc":"2.0","id":1,"method":"window/showDocument","params":{"uri":"file:///w/a.go","URI":"https://evil.example"}}`)); fwd != nil || reply != nil {
		t.Fatalf("a server message with a case-twin key was passed: %s %s", fwd, reply)
	}
}
