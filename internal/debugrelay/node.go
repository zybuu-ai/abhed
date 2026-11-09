package debugrelay

import (
	_ "embed" // the Node adapter and its shim
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Node's adapter, run with the sandbox's own node, and the shim it loads
// into the program: the program's inspector is driven in process by a
// worker thread over a pipe, so nothing listens on a port.
var (
	//go:embed node/adapter.js
	nodeAdapter string
	//go:embed node/shim.mjs
	nodeShim string
)

// What a Node program or test is run with (Request.Runner).
const (
	RunNode   = "node"
	RunTsx    = "tsx"
	RunTSNode = "ts-node"
	RunVitest = "vitest"
	RunJest   = "jest"
)

// exitNoNode is the adapter command's exit code when node is missing.
const exitNoNode = 96

// NoNode is the problem of node missing where the session's commands run.
func NoNode(image string) *Problem { return MissingAdapter(exitNoNode, image) }

// NodeAdapterCommand is the shell command that runs the Node adapter.
func NodeAdapterCommand() string {
	shim, _ := json.Marshal(nodeShim)
	src := strings.Replace(nodeAdapter, "__ABHED_SHIM__", string(shim), 1)
	return "command -v node >/dev/null 2>&1 || { echo 'abhed-debug: node is not on PATH' >&2; exit " + strconv.Itoa(exitNoNode) + "; }; " +
		"exec node -e " + shellQuote(src)
}

// planNode is a Node program, or tests, run as runner asks.
func planNode(r Request, workspace, cwd string, launch map[string]any, prefix []string) (Spec, error) {
	bad := func(format string, a ...any) error {
		return &Problem{Kind: KindArgs, Message: "node: " + fmt.Sprintf(format, a...)}
	}
	var argv []string
	var lookups []map[string]any
	// fromModules is a package's file, found in the nearest node_modules
	// from cwd up to the workspace by the adapter, as node would.
	fromModules := func(name, rel string) string {
		lookups = append(lookups, map[string]any{"index": len(argv), "name": name, "rel": rel})
		return filepath.Join("node_modules", rel)
	}
	needs := func(name string) {
		lookups = append(lookups, map[string]any{"index": -1, "name": name, "rel": name + "/package.json"})
	}
	runner := r.Runner
	if r.Program != "" {
		p, err := inside(workspace, resolve(cwd, r.Program))
		if err != nil {
			return Spec{}, bad("%v", err)
		}
		switch runner {
		case "", RunNode:
			runner, argv = RunNode, []string{p}
		case RunTsx:
			needs("tsx")
			argv = []string{"--import", "tsx", p}
		case RunTSNode:
			needs("ts-node")
			argv = []string{"--require", "ts-node/register", p}
		case RunVitest, RunJest:
			return Spec{}, bad("%s runs tests: name a test, not a program", runner)
		default:
			return Spec{}, bad("runner %q is not one of node, tsx, ts-node, vitest or jest", runner)
		}
	} else {
		switch runner {
		case "", RunNode:
			if _, err := inside(workspace, resolve(cwd, r.Test)); err != nil {
				return Spec{}, bad("%v", err)
			}
			// In this process, where the debugger is, not one per file.
			runner, argv = RunNode, []string{"--test", "--test-isolation=none", resolve(cwd, r.Test)}
		case RunVitest:
			// Threads, one at a time: worker threads are debugged, child
			// processes are not.
			argv = append(argv, fromModules("vitest", "vitest/vitest.mjs"))
			argv = append(argv, "run", "--pool=threads", "--no-file-parallelism", r.Test)
		case RunJest:
			argv = append(argv, fromModules("jest", "jest/bin/jest.js"))
			argv = append(argv, "--runInBand", r.Test)
		case RunTsx, RunTSNode:
			return Spec{}, bad("%s runs a program: name a program, not a test", runner)
		default:
			return Spec{}, bad("runner %q is not one of node, tsx, ts-node, vitest or jest", runner)
		}
	}
	argv = append(argv, r.Args...)
	launch["runner"], launch["argv"], launch["root"] = runner, argv, workspace
	if len(lookups) > 0 {
		launch["resolve"] = lookups
	}
	delete(launch, "args")
	display := strings.TrimSpace(strings.Join(prefix, " ") + " " + quoteAll(append([]string{"node"}, argv...)))
	return Spec{Cwd: cwd, Launch: launch, Display: display, Command: NodeAdapterCommand()}, nil
}
