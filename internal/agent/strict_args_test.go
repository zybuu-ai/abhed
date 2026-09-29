package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// rawCall is a tool call whose arguments are sent exactly as written.
func rawCall(name, args string) model.ToolCall {
	return model.ToolCall{ID: "c" + name, Name: name, Args: json.RawMessage(args)}
}

// refusedArgs runs one call and reports whether it was denied at the args step.
func refusedArgs(t *testing.T, l *Loop, store *MemStore) (denied bool, reason string) {
	t.Helper()
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("sess1")
	for _, e := range evs {
		if e.Type == EvActionApproved || e.Type == EvObservation {
			t.Fatalf("a call with ambiguous arguments was %s: %s", e.Type, e.Payload)
		}
		if e.Type == EvActionDenied {
			var d map[string]string
			_ = json.Unmarshal(e.Payload, &d)
			return d["step"] == "args", d["reason"]
		}
	}
	return false, ""
}

// The reviewed payload: policy read "echo safe", the tool ran touch.
func TestDuplicateCaseKeyCannotSmuggleACommand(t *testing.T) {
	dir := tempDir(t)
	pwned := filepath.Join(dir, "pwned")
	l, store := harnessIn(t, dir, []scriptedTurn{
		{calls: []model.ToolCall{rawCall("bash", `{"command":"echo safe","Command":"touch `+pwned+`","description":"x"}`)}},
		{text: "done"},
	}, policy.ModeDefault, false)
	if err := l.Policy.AddAllow("bash(echo *)"); err != nil {
		t.Fatal(err)
	}
	if denied, _ := refusedArgs(t, l, store); !denied {
		t.Fatal("want a denial at the args step")
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("the smuggled command ran")
	}
	adapter := l.Adapter.(*scriptedAdapter)
	last := adapter.gotRequests[len(adapter.gotRequests)-1]
	told := false
	for _, m := range last.Messages {
		if m.Role == model.RoleTool && strings.Contains(m.Content, "differ only in case") {
			told = true
		}
	}
	if !told {
		t.Fatal("the model was not told its arguments were malformed")
	}
}

func TestAmbiguousArgumentsAreRefusedBeforePolicy(t *testing.T) {
	dir := tempDir(t)
	secret := filepath.Join(dir, "secret", "f.txt")
	ok := filepath.Join(dir, "ok.txt")
	for name, c := range map[string]model.ToolCall{
		"same-case duplicate": rawCall("bash", `{"command":"echo a","command":"touch x","description":"x"}`),
		"escaped duplicate":   rawCall("bash", `{"command":"echo a","\u0063ommand":"touch x","description":"x"}`),
		"escaped case":        rawCall("bash", `{"command":"echo a","\u0043ommand":"touch x","description":"x"}`),
		"escaped lone case":   rawCall("bash", `{"\u0043OMMAND":"touch x","description":"x"}`),
		"unicode fold":        rawCall("bash", `{"command":"echo a","description":"x","deſcription":"y"}`),
		"lone case variant":   rawCall("bash", `{"COMMAND":"touch x","description":"x"}`),
		"write path case":     rawCall("write", `{"path":"`+ok+`","Path":"`+secret+`","content":"x"}`),
		"write lone Path":     rawCall("write", `{"PATH":"`+secret+`","content":"x"}`),
		"write with command":  rawCall("write", `{"command":"echo","path":"`+secret+`","content":"x"}`),
		"edit path case":      rawCall("edit", `{"path":"`+ok+`","pAth":"`+secret+`","old_string":"a","new_string":"b"}`),
		"read path case":      rawCall("read", `{"path":"`+ok+`","Path":"`+secret+`"}`),
		"nested duplicate":    rawCall("todo", `{"items":[{"id":"1","text":"a","status":"done","Status":"pending"}]}`),
		"trailing data":       rawCall("bash", `{"command":"echo a","description":"x"} {"command":"touch x"}`),
		"not an object":       rawCall("bash", `["echo a"]`),
	} {
		t.Run(name, func(t *testing.T) {
			l, store := harnessIn(t, dir, []scriptedTurn{{calls: []model.ToolCall{c}}, {text: "done"}}, policy.ModeBypass, true)
			l.Tools.Add(tools.Todo{})
			if err := l.Policy.AddDeny("write("+filepath.Join(dir, "secret")+"/**)", "edit("+filepath.Join(dir, "secret")+"/**)", "read("+filepath.Join(dir, "secret")+"/**)"); err != nil {
				t.Fatal(err)
			}
			if denied, why := refusedArgs(t, l, store); !denied {
				t.Fatalf("want a denial at the args step, got %q", why)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); err == nil {
		t.Fatal("a smuggled command ran")
	}
	if _, err := os.Stat(secret); err == nil {
		t.Fatal("a smuggled path was written")
	}
}

// spy is the write tool, keeping the bytes it was given.
type spy struct {
	tools.Write
	got []json.RawMessage
}

func (s *spy) Run(ctx context.Context, sess *tools.Session, args json.RawMessage) tools.Result {
	s.got = append(s.got, args)
	return s.Write.Run(ctx, sess, args)
}

// What policy judged is what the tool ran, what the record kept and what history holds.
func TestCanonicalArgumentsAreWhatRunsAndIsRecorded(t *testing.T) {
	dir := tempDir(t)
	out := filepath.Join(dir, "out.txt")
	l, store := harnessIn(t, dir, []scriptedTurn{
		{calls: []model.ToolCall{rawCall("write", `{"p\u0061th":"`+out+`","timeout":5,"content":"a<b>"}`)}},
		{text: "done"},
	}, policy.ModeBypass, true)
	w := &spy{}
	l.Tools.Add(w)
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	want := `{"content":"a<b>","path":"` + out + `"}`
	if len(w.got) != 1 || string(w.got[0]) != want {
		t.Fatalf("the tool got %s, want %s", w.got, want)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "a<b>" {
		t.Fatalf("wrote %q %v", b, err)
	}
	var escaped bytes.Buffer
	json.HTMLEscape(&escaped, []byte(want))
	evs, _ := store.Events("sess1")
	for _, e := range evs {
		if e.Type == EvActionRequested {
			var p ActionRequested
			_ = json.Unmarshal(e.Payload, &p)
			if string(p.Args) != escaped.String() || len(p.Dropped) != 1 || p.Dropped[0] != "timeout" {
				t.Fatalf("recorded %s dropping %v, want %s dropping timeout", p.Args, p.Dropped, want)
			}
		}
	}
	if got := string(l.Messages()[1].ToolCalls[0].Args); got != want {
		t.Fatalf("history holds %s, want %s", got, want)
	}
}

// remote stands in for an MCP tool: its arguments go to another process as sent.
type remote struct{ got []json.RawMessage }

func (*remote) Name() string        { return "mcp__srv__run" }
func (*remote) Description() string { return "runs a script on a server" }
func (*remote) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"Mode":{"type":"string"}}}`)
}
func (*remote) Mutates() bool { return true }
func (r *remote) Run(_ context.Context, _ *tools.Session, args json.RawMessage) tools.Result {
	r.got = append(r.got, args)
	return tools.Result{Content: "ok"}
}

func TestMCPArgumentsSentAreTheOnesJudged(t *testing.T) {
	for name, c := range map[string]struct {
		args   string
		refuse bool
	}{
		"duplicate":           {`{"path":"/ok","path":"/etc/x"}`, true},
		"case variant":        {`{"path":"/ok","PATH":"/etc/x"}`, true},
		"undeclared subject":  {`{"command":"ls","path":"/etc/x"}`, true},
		"declared spelling":   {`{"path":"/ok","extra":{"b":1,"a":"<2>"},"\u004dode":"fast"}`, false},
		"misspelled declared": {`{"path":"/ok","mode":"fast"}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := &remote{}
			store := NewMemStore()
			sess, _ := tools.NewSession(tempDir(t))
			pol := policy.New(policy.ModeBypass)
			if err := pol.AddDeny("mcp__srv__run(/etc/*)"); err != nil {
				t.Fatal(err)
			}
			l := NewLoop(&scriptedAdapter{turns: []scriptedTurn{{calls: []model.ToolCall{rawCall(r.Name(), c.args)}}, {text: "done"}}},
				tools.NewRegistry(r), pol, AutoApprove{Yes: true}, sess, NewRecorder(store, "sess1", ""), DefaultConfig())
			if _, err := l.Run(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			if c.refuse {
				if len(r.got) != 0 {
					t.Fatalf("sent %s", r.got[0])
				}
				return
			}
			if len(r.got) != 1 {
				t.Fatal("the call was not sent")
			}
			if want := `{"Mode":"fast","extra":{"a":"<2>","b":1},"path":"/ok"}`; string(r.got[0]) != want {
				t.Fatalf("sent %s, want %s", r.got[0], want)
			}
			if s := policy.Subject(r.Name(), r.got[0]); s != "/ok" {
				t.Fatalf("sent arguments whose subject is %q", s)
			}
		})
	}
}
