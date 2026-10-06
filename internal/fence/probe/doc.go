// Package probe qualifies a host for the fence tier. Before the fence runs a
// command, Run checks that this kernel really provides each protection the
// fence depends on, by trying it: a non-root user whose would-be child holds
// no capabilities, no_new_privs, a Landlock domain that allows a write in one
// folder and refuses one elsewhere (and refuses TCP when the network is off),
// a seccomp filter that takes effect, and a writable delegated cgroup v2
// subtree. It records the kernel release and architecture alongside.
//
// The checks that need a confined process run in a helper: this binary
// re-executed with HelperArg, which the package's init answers and exits, so
// the caller is never confined. A binary that does not link this package
// cannot answer, and every such check then fails.
//
// The Report says, check by check, pass, fail or skip with a reason and the
// value measured, and is Qualified only when every required check passed. Any
// failure refuses the fence: there is no fallback and no partial fence. Root
// is refused in this release. A pass is never cached; call Run each time.
//
// On anything but Linux, Run reports that the fence is not supported.
package probe
