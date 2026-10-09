package extension

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// providedTool is a tool an extension supplies, adapted to the tool interface.
//
// It is a tool like any other from the loop's point of view: it appears in the
// model's tool list, its call goes through the policy engine, and its result is
// recorded as an observation tagged untrusted. Providing a tool is a way to add
// a capability, never a way around the rules — a tool that mutates is subject
// to approval exactly as a built-in is, and a deny rule naming it still wins.
type providedTool struct {
	ext     *Extension
	def     ToolDef
	mutates bool
}

func (t providedTool) Name() string { return t.def.Name }

func (t providedTool) Description() string {
	return t.def.Description + " (provided by the " + t.ext.Name() + " extension)"
}

func (t providedTool) Schema() json.RawMessage {
	if len(t.def.Schema) == 0 {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return t.def.Schema
}

func (t providedTool) Mutates() bool { return t.mutates }

func (t providedTool) Run(ctx context.Context, _ *tools.Session, args json.RawMessage) tools.Result {
	reply := t.ext.Call(ctx, Request{
		Event: EvInvokeTool, Tool: t.def.Name, Args: args,
	})
	// A dead or silent extension returns an empty reply. Saying so is better
	// than an empty result the model reads as success.
	if reply.Result == "" && reply.Content == nil {
		return tools.Result{
			Content: fmt.Sprintf("The %s extension did not answer the %s call.",
				t.ext.Name(), t.def.Name),
			IsError: true,
		}
	}
	content := reply.Result
	if reply.Content != nil {
		content = *reply.Content
	}
	isErr := false
	if reply.IsError != nil {
		isErr = *reply.IsError
	}
	return tools.Result{Content: content, IsError: isErr}
}

// Tools asks every extension what tools it provides.
//
// Called once at startup: a tool list that changed mid-session would mean the
// model's prompt no longer matched what it could call, and the prefix cache
// would be invalidated on every change.
func (h *Host) Tools(ctx context.Context) ([]tools.Tool, []error) {
	var out []tools.Tool
	var errs []error
	seen := map[string]string{}

	for _, e := range h.exts {
		if !e.Subscribed(EvListTools) {
			continue
		}
		reply := e.Call(ctx, Request{Event: EvListTools})
		for _, def := range reply.Tools {
			if def.Name == "" {
				errs = append(errs, fmt.Errorf("extension %s: a tool with no name", e.Name()))
				continue
			}
			if policy.ReservedToolName(def.Name) {
				// A rule names a built-in in any case, so a tool spelled like one
				// would take a rule meant for the built-in, or replace it.
				errs = append(errs, fmt.Errorf(
					"extension %s: tool %q is refused: it is the name of a built-in tool, in any case", e.Name(), def.Name))
				continue
			}
			if owner, dup := seen[def.Name]; dup {
				// Two extensions claiming one name would make which runs
				// depend on load order, and a policy rule naming it ambiguous.
				errs = append(errs, fmt.Errorf(
					"extension %s: tool %q is already provided by %s",
					e.Name(), def.Name, owner))
				continue
			}
			seen[def.Name] = e.Name()

			// Abhed cannot know what someone else's tool does, so an
			// unspecified tool is assumed to mutate: that routes it through
			// approval rather than letting it run unattended.
			mutates := true
			if def.Mutates != nil {
				mutates = *def.Mutates
			}
			out = append(out, providedTool{ext: e, def: def, mutates: mutates})
		}
	}
	return out, errs
}
