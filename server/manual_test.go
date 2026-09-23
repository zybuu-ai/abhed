package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// manualBench is a workbench whose registry can also run commands.
func manualBench(t *testing.T, edit func(*config.Config)) *workbench {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	if edit != nil {
		edit(&cfg)
	}
	dir := t.TempDir()
	s := New(Options{
		Workspace: dir, Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Bash{}),
	})
	wb := &workbench{t: t, h: s.Handler(), workspace: dir}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"work"}`))
	req.Header.Set("X-Abhed-Tenant", "acme")
	wb.h.ServeHTTP(rec, req)
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	wb.session = created.SessionID
	return wb
}

func (wb *workbench) send(tenant, method, endpoint string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/v1/sessions/"+wb.session+"/"+endpoint, strings.NewReader(string(raw)))
	req.Header.Set("X-Abhed-Tenant", tenant)
	wb.h.ServeHTTP(rec, req)
	return rec
}

func (wb *workbench) events() []agent.Event {
	wb.t.Helper()
	var evs []agent.Event
	if err := json.Unmarshal(wb.get("acme", "replay").Body.Bytes(), &evs); err != nil {
		wb.t.Fatal(err)
	}
	return evs
}

// A save by hand lands on disk, in the changes view and in the record, as the
// person's own action.
func TestManualSaveIsWrittenDiffedAndRecorded(t *testing.T) {
	wb := manualBench(t, nil)
	wb.write("a.txt", "one\n")
	_, f := wb.file("a.txt")
	if f.Hash == "" {
		t.Fatal("a whole file must come with the hash a save is checked against")
	}

	rec := wb.send("acme", "PUT", "file", saveRequest{Path: "a.txt", Content: "one\ntwo\n", Base: f.Hash})
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	got, _ := os.ReadFile(filepath.Join(wb.workspace, "a.txt"))
	if string(got) != "one\ntwo\n" {
		t.Fatalf("on disk: %q", got)
	}
	var ch changesResponse
	_ = json.Unmarshal(wb.get("acme", "changes").Body.Bytes(), &ch)
	if len(ch.Files) != 1 || ch.Files[0].Path != "a.txt" || ch.Files[0].Added != 1 {
		t.Fatalf("changes view does not show the save: %+v", ch.Files)
	}

	var asked, allowedByUser bool
	for _, e := range wb.events() {
		switch e.Type {
		case agent.EvActionRequested:
			asked = asked || e.Actor == agent.ActorUser
		case agent.EvActionApproved:
			allowedByUser = allowedByUser || strings.Contains(string(e.Payload), `"by":"user"`)
		}
	}
	if !asked || !allowedByUser {
		t.Fatalf("the record must show the person asked and was the one who allowed it (asked=%v by-user=%v)", asked, allowedByUser)
	}
}

func TestManualSaveRefusesAStaleEditor(t *testing.T) {
	wb := manualBench(t, nil)
	wb.write("a.txt", "one\n")
	_, f := wb.file("a.txt")
	wb.write("a.txt", "changed underneath\n")

	rec := wb.send("acme", "PUT", "file", saveRequest{Path: "a.txt", Content: "mine\n", Base: f.Hash})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale save: %d %s", rec.Code, rec.Body)
	}
	got, _ := os.ReadFile(filepath.Join(wb.workspace, "a.txt"))
	if string(got) != "changed underneath\n" {
		t.Fatalf("a refused save still wrote: %q", got)
	}
}

// The person holds no power the agent lacks: the server's own state, paths
// outside the workspace, deny rules and other tenants' sessions all refuse.
func TestManualActionsAreHeldToTheSameRules(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "write(**/locked/**)", "read(**/.env)")
	})
	wb.write(".abhed/config.json", "{}")
	wb.write("locked/x.txt", "keep\n")
	wb.write(".env", "SECRET=1\n")
	outside := filepath.Join(filepath.Dir(wb.workspace), "outside.txt")

	for name, req := range map[string]saveRequest{
		"server state":   {Path: ".abhed/config.json", Content: "{\"x\":1}"},
		"new in state":   {Path: ".abhed/users.json", Content: "[]"},
		"traversal":      {Path: "../outside.txt", Content: "x"},
		"absolute":       {Path: outside, Content: "x"},
		"read-denied":    {Path: ".env", Content: "SECRET=2\n"},
		"repository dir": {Path: ".git/config", Content: "x"},
	} {
		if rec := wb.send("acme", "PUT", "file", req); rec.Code == http.StatusOK {
			t.Errorf("%s: a save by hand was accepted: %s", name, rec.Body)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a save escaped the workspace")
	}

	_, f := wb.file("locked/x.txt")
	rec := wb.send("acme", "PUT", "file", saveRequest{Path: "locked/x.txt", Content: "gone\n", Base: f.Hash})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Denied") {
		t.Fatalf("write deny rule: %d %s", rec.Code, rec.Body)
	}
	got, _ := os.ReadFile(filepath.Join(wb.workspace, "locked/x.txt"))
	if string(got) != "keep\n" {
		t.Fatalf("a denied save still wrote: %q", got)
	}

	rec = wb.send("acme", "POST", "exec", execRequest{Command: "shutdown -h now"})
	var out execResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.IsError || !strings.Contains(out.Output, "Denied") {
		t.Fatalf("the shipped deny rule did not hold for a typed command: %s", rec.Body)
	}
	denied := false
	for _, e := range wb.events() {
		denied = denied || e.Type == agent.EvActionDenied
	}
	if !denied {
		t.Fatal("a refused command must be in the record")
	}

	for _, probe := range []*httptest.ResponseRecorder{
		wb.send("other", "PUT", "file", saveRequest{Path: "b.txt", Content: "x"}),
		wb.send("other", "POST", "exec", execRequest{Command: "pwd"}),
	} {
		if probe.Code == http.StatusOK {
			t.Fatalf("another tenant acted on this session: %s", probe.Body)
		}
	}
}

// The person's terminal keeps its own directory, so a cd never moves the agent.
func TestManualCommandRunsAndKeepsItsOwnDirectory(t *testing.T) {
	wb := manualBench(t, nil)
	wb.write("sub/f.txt", "x")

	wb.send("acme", "POST", "exec", execRequest{Command: "cd sub"})
	rec := wb.send("acme", "POST", "exec", execRequest{Command: "ls"})
	var out execResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || !strings.Contains(out.Output, "f.txt") || out.Cwd != "sub" {
		t.Fatalf("exec: %d %+v", rec.Code, out)
	}
}

// A person's save of work in progress is never refused for its syntax: it is
// saved, and the warning comes back with it.
func TestManualSaveOfBrokenCodeIsWarnedNotRefused(t *testing.T) {
	wb := manualBench(t, nil)
	wb.write("main.go", "package main\n")
	_, f := wb.file("main.go")
	rec := wb.send("acme", "PUT", "file", saveRequest{Path: "main.go", Content: "package main\nfunc (\n", Base: f.Hash})
	if rec.Code != http.StatusOK {
		t.Fatalf("a save of broken code was refused: %d %s", rec.Code, rec.Body)
	}
	got, _ := os.ReadFile(filepath.Join(wb.workspace, "main.go"))
	if string(got) != "package main\nfunc (\n" {
		t.Fatalf("on disk: %q", got)
	}
	warned := false
	for _, e := range wb.events() {
		warned = warned || (e.Type == agent.EvObservation && strings.Contains(string(e.Payload), "no longer parses"))
	}
	if !warned {
		t.Fatal("the record does not carry the syntax warning")
	}
}
