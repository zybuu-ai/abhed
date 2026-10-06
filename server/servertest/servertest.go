// Package servertest builds servers for tests outside the server package.
//
// The server's own tests reach unexported fields; a test in another package —
// an edition exercising the routes it mounts — cannot, and must not need to.
// What every such test needs is the same: a server with a model that answers
// instantly and a tool registry with something in it. This is that, and
// nothing that would let a test depend on how the server is put together.
package servertest

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/server"
)

// StubAdapter answers every completion with "done" and no tool calls, so a
// session started in a test finishes on its own.
type StubAdapter struct{}

func (StubAdapter) Name() string { return "stub" }
func (StubAdapter) Profile() model.Profile {
	return model.Profile{Name: "stub", ContextWindow: 32000}
}
func (StubAdapter) CountTokens(model.Request) (int, error) { return 10, nil }
func (StubAdapter) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 2)
	ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

// Options are the defaults every test server starts from: a temporary
// workspace, the default config, the stub model and two read-only tools.
// Callers adjust them before New.
func Options(t testing.TB) server.Options {
	t.Helper()
	return server.Options{
		Workspace: t.TempDir(),
		Config:    config.Default(),
		Adapter:   StubAdapter{},
		Registry:  tools.NewRegistry(tools.Read{}, tools.Glob{}),
	}
}

// New builds a server with authentication off, after letting each adjust
// function change the options.
func New(t testing.TB, adjust ...func(*server.Options)) *server.Server {
	t.Helper()
	o := Options(t)
	for _, f := range adjust {
		f(&o)
	}
	return server.New(o)
}

// Proxy builds a server that trusts X-Abhed-* headers, the deployment shape
// where a trusted reverse proxy has already authenticated the caller. It is
// the easiest way for a test to be somebody: set X-Abhed-User and, for an
// administrator, X-Abhed-Groups.
func Proxy(t testing.TB, adjust ...func(*server.Options)) *server.Server {
	t.Helper()
	return New(t, append([]func(*server.Options){func(o *server.Options) {
		o.Config.Auth.Mode = "proxy"
	}}, adjust...)...)
}

// Turn is one scripted reply: Text, or a call of Tool with Args.
type Turn struct {
	Text string
	Tool string
	Args map[string]any
}

// Scripted answers each completion with what Reply returns for the
// conversation: its first message, its latest, and whether the latest is a
// tool result. It counts the completions it served.
type Scripted struct {
	Reply func(first, last string, fromTool bool) Turn

	mu    sync.Mutex
	calls int
}

func (*Scripted) Name() string { return "scripted" }
func (*Scripted) Profile() model.Profile {
	return model.Profile{Name: "scripted", ContextWindow: 32000}
}
func (*Scripted) CountTokens(model.Request) (int, error) { return 10, nil }
func (s *Scripted) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	first, last := req.Messages[0], req.Messages[len(req.Messages)-1]
	turn := s.Reply(first.Content, last.Content, last.Role == model.RoleTool)
	ch := make(chan model.Chunk, 2)
	if turn.Tool != "" {
		args, _ := json.Marshal(turn.Args)
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &model.ToolCall{ID: "call-" + strconv.Itoa(n), Name: turn.Tool, Args: args}}
	} else {
		ch <- model.Chunk{Type: model.ChunkText, Text: turn.Text}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

// Calls is how many completions were served.
func (s *Scripted) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// WithShellAndWrite adds bash and write to the tools, for a test whose
// agent starts commands and changes files.
func WithShellAndWrite(o *server.Options) {
	o.Registry = tools.NewRegistry(tools.Read{}, tools.Glob{}, tools.Bash{}, tools.Write{})
}

// WithSecrets gives the tools and the record the secrets store at path, as a
// served deployment's are: bash offers its names and reads its values, and
// the record is redacted with it. It follows WithShellAndWrite.
func WithSecrets(path string) func(*server.Options) {
	return func(o *server.Options) {
		v := secrets.Open(path)
		o.Registry = tools.BindStores(o.Registry, v)
		o.Redact = v.Live()
	}
}

// Event is one event a session recorded, as Events keeps it.
type Event struct {
	Type    string
	Payload json.RawMessage
}

// Events keeps every event the server appends, by session.
type Events struct {
	mu sync.Mutex
	by map[string][]Event
}

// Tap makes o's server report its events to e.
func (e *Events) Tap(o *server.Options) {
	o.EventTap = func(ev agent.Event) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.by == nil {
			e.by = map[string][]Event{}
		}
		e.by[ev.SessionID] = append(e.by[ev.SessionID], Event{Type: string(ev.Type), Payload: append(json.RawMessage(nil), ev.Payload...)})
	}
}

// Of returns the events session recorded so far, in order.
func (e *Events) Of(session string) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Event(nil), e.by[session]...)
}

// Count is how many events of typ session recorded.
func (e *Events) Count(session, typ string) int {
	n := 0
	for _, ev := range e.Of(session) {
		if ev.Type == typ {
			n++
		}
	}
	return n
}
