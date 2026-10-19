package app

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/ocsf"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// secretRedactor stands in for the vault's: one stored secret, redacted.
type secretRedactor struct{}

func (secretRedactor) Redact(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("sk-LOGS-CANARY-77"), []byte("[REDACTED:token]"))
}
func (secretRedactor) Span() int { return len("sk-LOGS-CANARY-77") }

// logsRecord opens the default local record under a fresh HOME and starts a
// session in workspace ws.
func logsRecord(t *testing.T, ws string) (*local.Store, *agent.Recorder, string) {
	t.Helper()
	rec, err := local.Open(local.Options{Tenant: "default", User: cliUser(), Redact: secretRedactor{}})
	if err != nil {
		t.Fatal(err)
	}
	id := newConversationID()
	if err := rec.CreateSession(context.Background(), store.SessionRecord{ID: id, Workspace: ws, User: cliUser()}); err != nil {
		t.Fatal(err)
	}
	return rec, agent.NewRecorder(rec, id, ""), id
}

func logSay(t *testing.T, r *agent.Recorder, text string) {
	t.Helper()
	if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: text}); err != nil {
		t.Fatal(err)
	}
}

func runLogsOut(t *testing.T, ws string, args ...string) (string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runLogs(context.Background(), ws, args, config.TrustAsStored, &out, &errb)
	if code != 0 {
		t.Logf("stderr: %s", errb.String())
	}
	return out.String(), code
}

// A reader that stops and starts again with the same cursor file goes on
// where it stopped: nothing twice, nothing missed.
func TestLogsCursorSurvivesARestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	rec, r, id := logsRecord(t, ws)
	defer rec.Close()
	logSay(t, r, "one")
	logSay(t, r, "two")
	cur := filepath.Join(t.TempDir(), "cursor.json")

	out, code := runLogsOut(t, ws, id, "-format", "ocsf", "-cursor", cur)
	if code != 0 {
		t.Fatal(code)
	}
	first := ocsfLines(t, out)
	if len(first) != 2 {
		t.Fatalf("first read: %d events\n%s", len(first), out)
	}
	logSay(t, r, "three")
	out, _ = runLogsOut(t, ws, id, "-format", "ocsf", "-cursor", cur)
	again := ocsfLines(t, out)
	if len(again) != 1 || again[0].Seq != first[1].Seq+1 {
		t.Fatalf("after the restart: %+v", again)
	}
	out, _ = runLogsOut(t, ws, id, "-format", "ocsf", "-cursor", cur)
	if strings.TrimSpace(out) != "" {
		t.Fatalf("a third read with nothing new printed %s", out)
	}
}

// The log holds what the record holds: a secret the record redacted is
// redacted in every format.
func TestLogsRedactAsTheRecordDoes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	rec, r, id := logsRecord(t, ws)
	defer rec.Close()
	logSay(t, r, "the key is sk-LOGS-CANARY-77")
	for _, format := range []string{"ocsf", "record", "shorthand"} {
		out, code := runLogsOut(t, ws, id, "-format", format)
		if code != 0 {
			t.Fatal(format, code)
		}
		if strings.Contains(out, "sk-LOGS-CANARY-77") {
			t.Fatalf("-format %s printed the secret: %s", format, out)
		}
		if format != "shorthand" && !strings.Contains(out, "[REDACTED:token]") {
			t.Fatalf("-format %s lost the redacted text: %s", format, out)
		}
	}
}

func TestLogsFiltersAndDowngrade(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	rec, r, id := logsRecord(t, ws)
	defer rec.Close()
	logSay(t, r, "one")
	if _, err := r.Record(agent.EvAgentMessage, agent.ActorAgent, agent.Trusted, agent.Message{Text: "two"}); err != nil {
		t.Fatal(err)
	}
	// The latest session in the workspace is read when none is named.
	out, _ := runLogsOut(t, ws, "-type", "agent.message", "-format", "ocsf", "-ocsf-version", "1.3.0")
	evs := ocsfLines(t, out)
	if len(evs) != 1 || evs[0].Type != agent.EvAgentMessage {
		t.Fatalf("-type: %s", out)
	}
	if !strings.Contains(out, `"version":"1.3.0"`) {
		t.Fatalf("-ocsf-version: %s", out)
	}
	if out, _ := runLogsOut(t, ws, id, "-since", time.Now().Add(time.Hour).Format(time.RFC3339)); strings.TrimSpace(out) != "" {
		t.Fatalf("-since in the future printed %s", out)
	}
	out, _ = runLogsOut(t, ws, id, "-after", "1")
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "#2 ") {
		t.Fatalf("-after 1: %s", out)
	}
	for _, bad := range [][]string{{"-format", "xml"}, {"-ocsf-version", "1.4.0"}, {"-since", "yesterday"}, {"-all", id}, {"a", "b"}} {
		if _, code := runLogsOut(t, ws, bad...); code != 2 {
			t.Errorf("%v: exit %d, want 2", bad, code)
		}
	}
}

// -f prints each line as it is recorded, by another process as much as this one.
func TestLogsFollow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	old := logsPoll
	logsPoll = 20 * time.Millisecond
	t.Cleanup(func() { logsPoll = old })
	ws := t.TempDir()
	rec, r, id := logsRecord(t, ws)
	defer rec.Close()
	logSay(t, r, "before")
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuffer{}
	done := make(chan int)
	go func() { done <- runLogs(ctx, ws, []string{id, "-f"}, config.TrustAsStored, out, &bytes.Buffer{}) }()
	logSay(t, r, "after")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "#2 ") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := out.String(); !strings.Contains(got, "#1 ") || !strings.Contains(got, "#2 ") {
		t.Fatalf("followed: %s", got)
	}
}

// record.ocsf.enabled has the record the CLI opens write its OCSF file.
func TestOpenRecordWritesTheOCSFFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Config{Record: config.RecordConfig{OCSF: config.OCSFConfig{Enabled: true}}}
	rec, err := openRecord(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id := newConversationID()
	if err := rec.CreateSession(context.Background(), store.SessionRecord{ID: id, Workspace: t.TempDir(), User: cliUser()}); err != nil {
		t.Fatal(err)
	}
	logSay(t, agent.NewRecorder(rec, id, ""), "hello")
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".abhed", "records", "default", "ocsf", "events.ocsf.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	evs := ocsfLines(t, string(data))
	if len(evs) != 1 || evs[0].SessionID != id {
		t.Fatalf("the OCSF file holds %+v", evs)
	}
}

func ocsfLines(t *testing.T, out string) []ocsf.Line {
	t.Helper()
	var evs []ocsf.Line
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		l, err := ocsf.ReplayJSON(sc.Bytes())
		if err != nil {
			t.Fatalf("%v: %s", err, sc.Text())
		}
		evs = append(evs, l)
	}
	return evs
}
