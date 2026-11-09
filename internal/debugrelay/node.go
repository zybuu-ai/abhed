package debugrelay

import (
	_ "embed" // the Node adapter and its shim
	"encoding/json"
	"fmt"
	"os"
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

// KindRunner is a Node runner's package not installed in the workspace.
const KindRunner = "runner-missing"

// exitNoNode is the adapter command's exit code when node is missing.
const exitNoNode = 96

// NoNode is the problem of node missing where the session's commands run.
func NoNode(image string) *Problem { return MissingAdapter(exitNoNode, image) }

// NodeAdapterCommand is the shell command that runs the Node adapter.
func NodeAdapterCommand() string {
	shim, _ := json.Marshal(nodeShim)
	src := strings.Replace(nodeAdapter, "__ABHED_SHIM__", string(shim), 1)
	// The adapter runs with no NODE_OPTIONS, so nothing inherited (an
	// --inspect) opens a port in it; the program gets them back, filtered.
	return "command -v node >/dev/null 2>&1 || { echo 'abhed-debug: node is not on PATH' >&2; exit " + strconv.Itoa(exitNoNode) + "; }; " +
		"unset ABHED_NODE_OPTIONS; if [ -n \"${NODE_OPTIONS+x}\" ]; then ABHED_NODE_OPTIONS=$NODE_OPTIONS; export ABHED_NODE_OPTIONS; fi; " +
		"unset NODE_OPTIONS NODE_INSPECT_RESUME_ON_START; exec node -e " + shellQuote(src)
}

// planNode is a Node program, or tests, run as runner asks.
func planNode(r Request, workspace, cwd string, launch map[string]any, prefix []string) (Spec, error) {
	bad := func(format string, a ...any) error {
		return &Problem{Kind: KindArgs, Message: "node: " + fmt.Sprintf(format, a...)}
	}
	var argv []string
	var used string
	// pkg is the runner's package, found here as node finds it: in the
	// nearest node_modules from cwd, which must be inside the workspace.
	pkg := func(name string) (string, error) {
		for d := cwd; ; d = filepath.Dir(d) {
			if _, err := os.Stat(filepath.Join(d, "node_modules", name, "package.json")); err == nil {
				used = filepath.Join(d, "node_modules", name)
				return used, nil
			}
			if d == workspace || d == filepath.Dir(d) {
				return "", &Problem{Kind: KindRunner, Retry: true, Message: name + " is not installed in this workspace",
					Steps: []string{"Install the workspace's packages (npm install), or add it: npm install -D " + name, "Start the debug session again"}}
			}
		}
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
			// node takes tsx from the nearest node_modules too: the one found.
			if _, err := pkg("tsx"); err != nil {
				return Spec{}, err
			}
			argv = []string{"--import", "tsx", p}
		case RunTSNode:
			dir, err := pkg("ts-node")
			if err != nil {
				return Spec{}, err
			}
			argv = []string{"--require", filepath.Join(dir, "register"), p}
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
			dir, err := pkg("vitest")
			if err != nil {
				return Spec{}, err
			}
			// Threads, one at a time: worker threads are debugged, child
			// processes are not.
			argv = []string{filepath.Join(dir, "vitest.mjs"), "run", "--pool=threads", "--no-file-parallelism", r.Test}
		case RunJest:
			dir, err := pkg("jest")
			if err != nil {
				return Spec{}, err
			}
			argv = []string{filepath.Join(dir, "bin", "jest.js"), "--runInBand", r.Test}
		case RunTsx, RunTSNode:
			return Spec{}, bad("%s runs a program: name a program, not a test", runner)
		default:
			return Spec{}, bad("runner %q is not one of node, tsx, ts-node, vitest or jest", runner)
		}
	}
	argv = append(argv, r.Args...)
	launch["runner"], launch["argv"] = runner, argv
	delete(launch, "args")
	display := strings.TrimSpace(strings.Join(prefix, " ") + " " + quoteAll(append([]string{"node"}, argv...)))
	return Spec{Cwd: cwd, Launch: launch, Display: display, Command: NodeAdapterCommand(), Runner: used}, nil
}
