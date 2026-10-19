package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

const dbPassword = "s3cret-Pa55word-for-db"

func ioHost(t *testing.T) (*ioServer, *Registry) {
	t.Helper()
	srv := startIOServer(t, "hunter2")
	t.Setenv("TEST_SSH_PW", "hunter2")
	reg, errs := NewRegistry([]HostConfig{{Name: "vm1", Addr: srv.addr, User: "tester",
		PasswordEnv: "TEST_SSH_PW", KnownHostsFile: knownHostsFor(t, srv.testServer)}})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	t.Cleanup(reg.Close)
	return srv, reg
}

// The stored secret reaches the command's stdin, with a final newline, and
// never the command line; what the command echoes of it is hidden.
func TestStdinSecretReachesStdinOnly(t *testing.T) {
	srv, reg := ioHost(t)
	tool := Tool{R: reg, Secret: stored(map[string]string{"DB_PW": dbPassword})}
	res := tool.Run(context.Background(), nil, json.RawMessage(`{"host":"vm1","command":"echo-stdin","stdin_secret":"DB_PW"}`))
	if res.IsError {
		t.Fatal(res.Content)
	}
	if strings.Contains(res.Content, dbPassword) || !strings.Contains(res.Content, "[secret DB_PW]") {
		t.Fatalf("the model was shown %q", res.Content)
	}
	cmds, stdins := srv.ran()
	if len(cmds) != 1 || cmds[0] != "echo-stdin" || stdins[0] != dbPassword+"\n" {
		t.Fatalf("ran %q with stdin %q", cmds, stdins)
	}
	res = tool.Run(context.Background(), nil, json.RawMessage(`{"host":"vm1","command":"hash-stdin","stdin_secret":"DB_PW"}`))
	sum := sha256.Sum256([]byte(dbPassword + "\n"))
	if !strings.Contains(res.Content, hex.EncodeToString(sum[:])) {
		t.Fatalf("hash-stdin: %s", res.Content)
	}
}

func TestStdinSecretRefusals(t *testing.T) {
	_, reg := ioHost(t)
	for _, tc := range []struct {
		tool Tool
		want string
	}{
		{Tool{R: reg}, "no secrets store"},
		{Tool{R: reg, Secret: stored(map[string]string{})}, "NOPE"},
	} {
		tool, want := tc.tool, tc.want
		res := tool.Run(context.Background(), nil, json.RawMessage(`{"host":"vm1","command":"echo-stdin","stdin_secret":"NOPE"}`))
		if !res.IsError || !strings.Contains(res.Content, want) {
			t.Errorf("got %q, want %q", res.Content, want)
		}
	}
	res := Tool{R: reg, Secret: stored(nil)}.Run(context.Background(), nil,
		json.RawMessage(`{"host":"vm1","command":"echo-stdin","stdin_secret":"a password!"}`))
	if !res.IsError || !strings.Contains(res.Content, "NAME of a stored secret") {
		t.Errorf("a value as the name: %q", res.Content)
	}
}

type scripted struct {
	args string
	n    int
}

func (*scripted) Name() string                           { return "scripted" }
func (*scripted) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (*scripted) CountTokens(model.Request) (int, error) { return 0, nil }
func (o *scripted) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 2)
	if o.n == 0 {
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &model.ToolCall{ID: "c1", Name: "ssh", Args: json.RawMessage(o.args)}}
	} else {
		ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	}
	o.n++
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
	close(ch)
	return ch, nil
}

type seen struct{ reasons []string }

func (s *seen) Approve(_ context.Context, _ string, _ json.RawMessage, res policy.Result) (bool, error) {
	s.reasons = append(s.reasons, res.Reason)
	return true, nil
}

// Through the loop: the secret needs its own rule, the approval names it and
// not its value, and nothing recorded holds the value.
func TestStdinSecretThroughTheLoop(t *testing.T) {
	run := func(pol *policy.Engine, args string) (*ioServer, *seen, []agent.Event) {
		srv, reg := ioHost(t)
		ap := &seen{}
		store := agent.NewMemStore()
		sess, _ := tools.NewSession(t.TempDir())
		l := agent.NewLoop(&scripted{args: args}, tools.NewRegistry(Tool{R: reg, Secret: stored(map[string]string{"DB_PW": dbPassword})}),
			pol, ap, sess, agent.NewRecorder(store, "s", ""), agent.DefaultConfig())
		if _, err := l.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		evs, _ := store.Events("s")
		return srv, ap, evs
	}
	args := `{"host":"vm1","command":"echo-stdin","stdin_secret":"DB_PW"}`
	srv, ap, _ := run(policy.New(policy.ModeDefault), args)
	if cmds, _ := srv.ran(); len(cmds) != 0 || len(ap.reasons) != 0 {
		t.Fatalf("ran without secret(DB_PW): %v %v", cmds, ap.reasons)
	}
	pol := policy.New(policy.ModeDefault)
	_ = pol.AddAllow("secret(DB_PW)")
	srv, ap, evs := run(pol, args)
	if cmds, _ := srv.ran(); len(cmds) != 1 || len(ap.reasons) != 1 ||
		!strings.Contains(ap.reasons[0], "writes stored secret DB_PW to the command's stdin") {
		t.Fatalf("ran %v, asked %v", cmds, ap.reasons)
	}
	for _, ev := range evs {
		if strings.Contains(string(ev.Payload), dbPassword) {
			t.Errorf("%s holds the value: %s", ev.Type, ev.Payload)
		}
	}
}
