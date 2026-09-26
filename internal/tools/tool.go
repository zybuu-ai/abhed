// Package tools implements Abhed's native tool set.
//
// The contracts here are specified in docs/architecture/06-tool-contracts.md.
// Two rules drive most of this code:
//
//   - Errors are instructions. An error message is read by the model, not a
//     human, so it must say what failed and what to do instead. A dead-end
//     error costs a turn; a recoverable one costs nothing.
//   - No silent success. A tool that changed nothing must say so, or the model
//     proceeds on a false premise and the failure surfaces many turns later.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool is one callable capability exposed to the model.
type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	// Mutates reports whether a call can change state. Mutating tools route
	// through the policy engine for approval (docs P7).
	Mutates() bool
	Run(ctx context.Context, sess *Session, args json.RawMessage) Result
}

// Prechecker is an optional check that needs no side effect to make. The loop
// runs it before asking a person, so nobody approves a call that cannot succeed.
type Prechecker interface {
	Precheck(sess *Session, args json.RawMessage) error
}

// precheckPath is the shared check for tools whose target is a "path" argument.
func precheckPath(s *Session, raw json.RawMessage) error {
	var a struct {
		Path string `json:"path"`
	}
	if s == nil {
		return nil
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	_, err := s.Resolve(a.Path)
	return err
}

// Result is what the model sees. Content is rendered into the transcript, so
// it is written for the model to act on.
type Result struct {
	Content   string
	IsError   bool
	Truncated bool
	ExitCode  *int
	// Final asks the loop to end the run, completed, once this result has been
	// recorded. It exists for tools that ARE the answer — a structured-output
	// contract is delivered by calling a tool, and the run is over the moment
	// that call is accepted. Without it the model would have to say something
	// afterwards to terminate the turn, and that something is not the answer.
	Final bool
	// Tier is the sandbox tier a command ran under, for the record; see Bash.tier.
	Tier string
}

func ok(format string, a ...any) Result {
	return Result{Content: fmt.Sprintf(format, a...)}
}

// errf returns a model-facing error. Every message should name the problem and
// the next action; see the error tables in docs/architecture/06-tool-contracts.md.
func errf(format string, a ...any) Result {
	return Result{Content: fmt.Sprintf(format, a...), IsError: true}
}

// Registry holds the tools available to a session.
type Registry struct {
	tools map[string]Tool
	order []string
}

func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{tools: make(map[string]Tool)}
	for _, t := range ts {
		r.Add(t)
	}
	return r
}

func (r *Registry) Add(t Tool) {
	if _, dup := r.tools[t.Name()]; !dup {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, found := r.tools[name]
	return t, found
}

// Names returns tool names in registration order, so the prompt's tool list is
// stable across turns and does not invalidate the prefix cache (docs P8).
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.tools[n])
	}
	return out
}

// Clone returns an independent copy.
//
// This is what makes the tool set changeable at runtime without a lock on the
// read path. A Registry is read on every turn of every session, from many
// goroutines; adding a mutex would put lock traffic in the agent's hot loop to
// serve a change that happens once in a while.
//
// Instead a settings change clones, mutates the clone, and swaps the pointer.
// Sessions already running keep the registry they started with — which is also
// the correct semantics, since a tool set that changed mid-session would mean
// the model was told about tools that were not there when it planned.
func (r *Registry) Clone() *Registry {
	out := &Registry{
		tools: make(map[string]Tool, len(r.tools)),
		order: make([]string, len(r.order)),
	}
	for k, v := range r.tools {
		out.tools[k] = v
	}
	copy(out.order, r.order)
	return out
}

// Remove drops a tool. Used when an MCP server is disconnected, whose tools
// must not outlive the connection that served them.
func (r *Registry) Remove(name string) {
	if _, found := r.tools[name]; !found {
		return
	}
	delete(r.tools, name)
	out := r.order[:0]
	for _, n := range r.order {
		if n != name {
			out = append(out, n)
		}
	}
	r.order = out
}

// Subset returns a registry limited to the named tools. Subagent profiles use
// this: a narrow role with a narrow tool set outperforms a general one, and
// read-only profiles must not be able to write (docs §07).
func (r *Registry) Subset(names ...string) *Registry {
	sub := &Registry{tools: make(map[string]Tool)}
	for _, n := range names {
		if t, found := r.tools[n]; found {
			sub.Add(t)
		}
	}
	return sub
}

// Definition is the JSON shape sent to the model.
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func (r *Registry) Definitions() []Definition {
	out := make([]Definition, 0, len(r.order))
	for _, n := range r.order {
		t := r.tools[n]
		out = append(out, Definition{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.Schema(),
		})
	}
	return out
}
