package server

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func (wb *workbench) startPTY(command string) ptyStartResponse {
	wb.t.Helper()
	rec := wb.send("acme", "POST", "pty", ptyStartRequest{Command: command, Cols: 80, Rows: 24})
	if rec.Code != http.StatusOK {
		wb.t.Fatalf("pty start: %d %s", rec.Code, rec.Body)
	}
	var out ptyStartResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// ptyOutput streams one command until it exits and returns what it wrote.
func (wb *workbench) ptyOutput(id string, input string) (string, string) {
	wb.t.Helper()
	srv := httptest.NewServer(wb.h)
	defer srv.Close()
	if input != "" {
		go func() {
			time.Sleep(300 * time.Millisecond)
			req, _ := http.NewRequest("POST", srv.URL+"/v1/sessions/"+wb.session+"/pty/"+id+"/input", strings.NewReader(input))
			req.Header.Set("X-Abhed-Tenant", "acme")
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions/"+wb.session+"/pty/"+id, nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		wb.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out strings.Builder
	exit := ""
	sc := bufio.NewScanner(resp.Body)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			switch event {
			case "out":
				b, _ := base64.StdEncoding.DecodeString(data)
				out.Write(b)
			case "exit":
				exit = data
			}
		}
	}
	return out.String(), exit
}

// A command on the terminal can take input, and what it wrote lands in the
// record as the person's own bash call.
func TestTerminalCommandTakesInputAndIsRecorded(t *testing.T) {
	wb := manualBench(t, nil)
	start := wb.startPTY(`read -p "name? " n; echo "hi $n"`)
	if start.Denied != "" || start.ID == "" {
		t.Fatalf("start: %+v", start)
	}
	out, exit := wb.ptyOutput(start.ID, "abhed\n")
	if !strings.Contains(out, "hi abhed") || exit != "0" {
		t.Fatalf("output %q exit %q", out, exit)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		recorded := false
		for _, e := range wb.events() {
			if e.Type == agent.EvObservation && strings.Contains(string(e.Payload), "hi abhed") && strings.Contains(string(e.Payload), "on a terminal") {
				recorded = true
			}
		}
		if recorded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the terminal command's output was not recorded")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The deny rules hold for a typed line before anything runs, and the person's
// cd moves only their own directory.
func TestTerminalHoldsTheRulesAndItsOwnDirectory(t *testing.T) {
	wb := manualBench(t, nil)
	if start := wb.startPTY("shutdown -h now"); !strings.Contains(start.Denied, "Denied") {
		t.Fatalf("shutdown was not denied: %+v", start)
	}
	wb.write("sub/f.txt", "x")
	if start := wb.startPTY("cd sub"); start.Cwd != "sub" {
		t.Fatalf("cd did not move the terminal: %+v", start)
	}
	if start := wb.startPTY("cd ../.."); start.Cwd != "sub" {
		t.Fatalf("cd left the workspace: %+v", start)
	}
	start := wb.startPTY("pwd")
	out, _ := wb.ptyOutput(start.ID, "")
	if !strings.Contains(out, filepath.Join(wb.workspace, "sub")) {
		t.Fatalf("pwd: %q", out)
	}
	if rec := wb.send("other", "POST", "pty", ptyStartRequest{Command: "pwd"}); rec.Code == http.StatusOK {
		t.Fatal("another tenant started a command on this session's terminal")
	}
}

func TestTerminalCommandCanBeKilled(t *testing.T) {
	wb := manualBench(t, nil)
	start := wb.startPTY("sleep 30")
	done := make(chan string, 1)
	go func() { _, exit := wb.ptyOutput(start.ID, ""); done <- exit }()
	time.Sleep(200 * time.Millisecond)
	if rec := wb.send("acme", "DELETE", "pty/"+start.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("kill: %d %s", rec.Code, rec.Body)
	}
	select {
	case exit := <-done:
		if exit == "0" {
			t.Fatal("a killed command reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not end after being killed")
	}
}

// Accepting moves the baseline the changes view diffs against, and is in the
// record; the file itself is untouched.
func TestAcceptMovesTheBaseline(t *testing.T) {
	wb := manualBench(t, nil)
	wb.write("a.txt", "one\n")
	_, f := wb.file("a.txt")
	if rec := wb.send("acme", "PUT", "file", saveRequest{Path: "a.txt", Content: "one\ntwo\n", Base: f.Hash}); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	var orig originalResponse
	_ = json.Unmarshal(wb.get("acme", "original?path=a.txt").Body.Bytes(), &orig)
	if orig.Content != "one\n" || !orig.Existed {
		t.Fatalf("original: %+v", orig)
	}

	if rec := wb.send("acme", "POST", "accept", acceptRequest{Path: "a.txt", Content: "one\ntwo\n"}); rec.Code != http.StatusNoContent {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body)
	}
	var ch changesResponse
	_ = json.Unmarshal(wb.get("acme", "changes").Body.Bytes(), &ch)
	if len(ch.Files) != 0 {
		t.Fatalf("an accepted change is still listed: %+v", ch.Files)
	}
	if got, _ := os.ReadFile(filepath.Join(wb.workspace, "a.txt")); string(got) != "one\ntwo\n" {
		t.Fatalf("accept touched the file: %q", got)
	}
	accepted := false
	for _, e := range wb.events() {
		accepted = accepted || (e.Type == agent.EvChangeAccepted && e.Actor == agent.ActorUser)
	}
	if !accepted {
		t.Fatal("the acceptance is not in the record")
	}
	if rec := wb.send("acme", "POST", "accept", acceptRequest{Path: ".abhed/config.json", Content: "{}"}); rec.Code == http.StatusNoContent {
		t.Fatal("accepted a change to the server's own state")
	}
}

// A file up to the editor's limit opens whole, with the hash a save needs.
func TestLargeFileIsEditable(t *testing.T) {
	wb := manualBench(t, nil)
	wb.write("big.txt", strings.Repeat("0123456789abcdef\n", (2<<20)/17))
	rec, f := wb.file("big.txt")
	if rec.Code != http.StatusOK || f.Truncated || f.Hash == "" {
		t.Fatalf("a 2 MB file is not editable: %d truncated=%v hash=%q", rec.Code, f.Truncated, f.Hash)
	}
	if r := wb.send("acme", "PUT", "file", saveRequest{Path: "big.txt", Content: f.Content + "end\n", Base: f.Hash}); r.Code != http.StatusOK {
		t.Fatalf("save of a 2 MB file: %d %s", r.Code, r.Body)
	}
}
