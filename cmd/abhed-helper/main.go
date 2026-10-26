// Command abhed-helper is the small Linux helper the engine ships beside
// itself and binds read-only into a container, where the engine's own
// binary cannot run (an engine in a VM, or on another host): the egress
// relay in front of a command (__abhed_egress_relay), the relay over an
// exec stream (__abhed_egress_relay_stdio), and the debug bridge
// (__abhed_dap_bridge). It does nothing else, and is pinned by checksum.
package main

import (
	"fmt"
	"os"

	"github.com/zybuu-ai/abhed/internal/dapbridge" // serves __abhed_dap_bridge from its init
	"github.com/zybuu-ai/abhed/internal/egress"
	_ "github.com/zybuu-ai/abhed/internal/stdiorelay" // serves __abhed_egress_relay_stdio from its init
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == egress.RelayArg {
		os.Exit(egress.RunRelay(os.Args[2:]))
	}
	fmt.Fprintln(os.Stderr, "abhed-helper is run by the Abhed engine inside a sandbox; it takes "+
		egress.RelayArg+", __abhed_egress_relay_stdio or "+dapbridge.Arg)
	os.Exit(2)
}
