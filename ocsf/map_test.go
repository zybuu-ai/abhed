package ocsf

import (
	"bytes"
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/internal/agent"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var testOpts = Options{ProductVersion: "1.2.9", Hostname: "host-1", Tenant: "default"}

// recordTypePatterns find where a record type is named in the source: a
// typed or untyped event constant, or a name passed straight to a recorder.
var recordTypePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bEv[A-Za-z0-9]*\s+(?:agent\.)?EventType\s*=\s*"([a-z_]+(?:\.[a-z_]+)*)"`),
	regexp.MustCompile(`(?m)^\s*(?:const\s+)?(?:Ev|Event)[A-Za-z0-9]*\s*=\s*"([a-z_]+(?:\.[a-z_]+)+)"`),
	regexp.MustCompile(`\brec\("([a-z_]+(?:\.[a-z_]+)+)"`),
}

// sourceTypes are the record types the module's source names.
func sourceTypes(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "web", "testdata", ".git", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, re := range recordTypePatterns {
			for _, m := range re.FindAllSubmatch(data, -1) {
				found[string(m[1])] = path
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// A record type added anywhere in the module needs a row in the table: a
// mapping, or an explicit reason it is not mapped.
func TestEveryRecordTypeIsMapped(t *testing.T) {
	found := sourceTypes(t)
	if len(found) < 60 {
		t.Fatalf("found only %d record types in the source; the patterns no longer match it", len(found))
	}
	for typ, where := range found {
		if _, ok := table[agent.EventType(typ)]; !ok {
			t.Errorf("%s (%s) has no row in the OCSF table: map it, or give the reason it is not mapped", typ, where)
		}
	}
	for typ := range table {
		if _, ok := found[string(typ)]; !ok {
			t.Errorf("the table maps %s, which the source no longer names", typ)
		}
	}
}

// Every mapped type has a sample, so the goldens cover it.
func TestEveryMappedTypeHasASample(t *testing.T) {
	have := map[agent.EventType]bool{}
	for _, s := range samples {
		have[s.typ] = true
	}
	for typ := range table {
		if !have[typ] {
			t.Errorf("%s has no sample in samples_test.go", typ)
		}
	}
}

func mapAll(t *testing.T) ([]Line, []Event) {
	t.Helper()
	m := NewMapper(testOpts)
	var lines []Line
	var evs []Event
	for _, l := range sampleLines() {
		ev, ok, err := m.Map(l)
		if err != nil {
			t.Fatalf("%s: %v", l, err)
		}
		_, skip, _ := Mapping(l.Type)
		if ok == (skip != "") {
			t.Fatalf("%s: mapped %v, but the table's reason to skip is %q", l, ok, skip)
		}
		if ok {
			lines, evs = append(lines, l), append(evs, ev)
		}
	}
	return lines, evs
}

func TestGoldenMapping(t *testing.T) {
	lines, evs := mapAll(t)
	var buf bytes.Buffer
	for i, ev := range evs {
		if err := Validate(ev); err != nil {
			t.Errorf("%s: %v", lines[i], err)
		}
		class, _ := intOf(ev["class_uid"])
		if want, _, _ := Mapping(lines[i].Type); !containsInt(want, class) {
			t.Errorf("%s: class %d is not one the table lists (%v)", lines[i], class, want)
		}
		writeJSONLine(t, &buf, ev)
	}
	golden(t, "testdata/golden-"+Version+".jsonl", buf.Bytes())
}

func TestGoldenShorthand(t *testing.T) {
	_, evs := mapAll(t)
	var buf bytes.Buffer
	for _, ev := range evs {
		line := Shorthand(ev)
		if strings.ContainsAny(line, "\n\r") || line == "" {
			t.Fatalf("shorthand %q is not one line", line)
		}
		buf.WriteString(line + "\n")
	}
	golden(t, "testdata/shorthand.txt", buf.Bytes())
}

func TestDowngrade(t *testing.T) {
	lines, evs := mapAll(t)
	for _, v := range Versions[1:] {
		var buf bytes.Buffer
		for i, ev := range evs {
			down, err := Downgrade(ev, v)
			if err != nil {
				t.Fatalf("%s to %s: %v", lines[i], v, err)
			}
			if err := Validate(down); err != nil {
				t.Errorf("%s to %s: %v", lines[i], v, err)
			}
			// Nothing is lost: the line replays from the older event as well.
			back, err := Replay(down)
			if err != nil {
				t.Fatalf("%s to %s: replay: %v", lines[i], v, err)
			}
			if !sameLine(back, lines[i]) {
				t.Errorf("%s to %s: replayed %+v", lines[i], v, back)
			}
			writeJSONLine(t, &buf, down)
		}
		golden(t, "testdata/golden-"+v+".jsonl", buf.Bytes())
	}
}

// What 1.5.0 lacks of an AI call's event is kept, not dropped.
func TestDowngradeKeepsWhatTheOlderVersionLacks(t *testing.T) {
	m := NewMapper(testOpts)
	for _, l := range sampleLines() {
		m.Observe(l)
	}
	l := sampleLines()[14] // model.call
	ev, _, _ := m.Map(l)
	if _, ok := ev["ai_model"]; !ok {
		t.Fatalf("model.call carries no ai_model: %v", ev)
	}
	down, err := Downgrade(ev, "1.5.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := down["ai_model"]; ok {
		t.Fatal("1.5.0 has no ai_model, and the downgrade kept it at the top")
	}
	kept := down["unmapped"].(map[string]any)["downgraded"].(map[string]any)
	if kept["from"] != Version || kept["attributes"].(map[string]any)["ai_model"] == nil {
		t.Fatalf("the downgrade did not keep ai_model: %v", kept)
	}
	if _, err := Downgrade(down, Version); err == nil {
		t.Fatal("an upgrade was written")
	}
	if _, err := Downgrade(ev, "1.4.0"); err == nil {
		t.Fatal("a version the subset does not hold was written")
	}
}

func TestReplayRebuildsTheLine(t *testing.T) {
	lines, evs := mapAll(t)
	for i, ev := range evs {
		var buf bytes.Buffer
		writeJSONLine(t, &buf, ev)
		back, err := ReplayJSON(buf.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if !sameLine(back, lines[i]) {
			t.Errorf("%s: replayed %+v", lines[i], back)
		}
	}
}

// A type the table does not know is still written, as a Base Event.
func TestUnknownTypeIsABaseEvent(t *testing.T) {
	l := sampleLines()[0]
	l.Type = "something.new"
	ev, ok, err := NewMapper(testOpts).Map(l)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if c, _ := intOf(ev["class_uid"]); c != 0 {
		t.Fatalf("class %d", c)
	}
	if err := Validate(ev); err != nil {
		t.Fatal(err)
	}
}

// The provider a model call's ai_model names comes from earlier in the
// session: without it, ai_model is left out rather than guessed.
func TestModelCallWithoutAProviderLeavesAIModelOut(t *testing.T) {
	l := sampleLines()[14]
	ev, _, _ := NewMapper(testOpts).Map(l)
	if _, ok := ev["ai_model"]; ok {
		t.Fatal("ai_model written without a provider")
	}
	if err := Validate(ev); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefusesAnInventedField(t *testing.T) {
	_, evs := mapAll(t)
	ev, _ := normal(evs[0])
	ev["agent_mood"] = "fine"
	if err := Validate(ev); err == nil || !strings.Contains(err.Error(), "agent_mood is not defined") {
		t.Fatalf("got %v", err)
	}
	ev, _ = normal(evs[0])
	delete(ev, "metadata")
	if err := Validate(ev); err == nil {
		t.Fatal("an event without metadata validated")
	}
}

func sameLine(a, b Line) bool {
	if a.ID != b.ID || a.SessionID != b.SessionID || a.Seq != b.Seq || a.Type != b.Type || a.Actor != b.Actor ||
		a.Trust != b.Trust || !a.CreatedAt.Equal(b.CreatedAt) || a.ParentID != b.ParentID || a.Prev != b.Prev || a.Hash != b.Hash {
		return false
	}
	return bytes.Equal(canonJSON(a.Payload), canonJSON(b.Payload))
}

func canonJSON(raw []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	_ = dec.Decode(&v)
	out, _ := json.Marshal(v)
	return out
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func writeJSONLine(t *testing.T, buf *bytes.Buffer, ev Event) {
	t.Helper()
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ev); err != nil {
		t.Fatal(err)
	}
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./ocsf -update to write it)", err)
	}
	if !bytes.Equal(got, want) {
		g, w := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(g) || i < len(w); i++ {
			var gl, wl string
			if i < len(g) {
				gl = g[i]
			}
			if i < len(w) {
				wl = w[i]
			}
			if gl != wl {
				t.Fatalf("%s differs at line %d:\n got %s\nwant %s\n(run go test ./ocsf -update if the change is meant)", path, i+1, gl, wl)
			}
		}
	}
}

// A control-plane row is API Activity in every version, its principal the actor.
func TestMapAudit(t *testing.T) {
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, a := range []controlplane.AuditEvent{
		{Seq: 1, At: at, Principal: "alice", Action: "create", SandboxID: "sb-1", Outcome: controlplane.OutcomeRequested},
		{Seq: 2, At: at, Principal: "alice", Action: "template.delete", SandboxID: "", Outcome: controlplane.OutcomeOK},
		{Seq: 3, At: at, Principal: controlplane.DriverPrincipal, Action: "sandbox.snapshot", SandboxID: "sb-1", Outcome: controlplane.OutcomeOK,
			Detail: map[string]string{"snapshot": "s1"}},
		{Seq: 4, At: at, Principal: "bob", Action: "exec", SandboxID: "sb-1", Outcome: controlplane.OutcomeFailed, Code: "denied"},
	} {
		ev := MapAudit(a, testOpts)
		for _, v := range Versions {
			down, err := Downgrade(ev, v)
			if err != nil {
				t.Fatal(err)
			}
			if err := Validate(down); err != nil {
				t.Errorf("%s in %s: %v", a.Action, v, err)
			}
		}
		user := ev["actor"].(map[string]any)["user"].(map[string]any)
		if user["name"] != a.Principal {
			t.Errorf("%s: actor %v", a.Action, user)
		}
	}
	if act := MapAudit(controlplane.AuditEvent{Action: "template.delete", At: at}, testOpts)["activity_id"]; act != 4 {
		t.Fatalf("template.delete is activity %v", act)
	}
}
