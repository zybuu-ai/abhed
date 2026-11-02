//go:build darwin && cgo

package sandbox

/*
#include <sys/types.h>
extern int sandbox_check(pid_t pid, const char *operation, int type, ...);
static int abhed_sandboxed(pid_t pid) { return sandbox_check(pid, 0, 0); }
*/
import "C"

// processSandboxed reports whether pid runs under any sandbox profile;
// known is false where that cannot be told.
func processSandboxed(pid int) (sandboxed, known bool) {
	if pid <= 1 {
		return false, false
	}
	switch C.abhed_sandboxed(C.pid_t(pid)) {
	case 1:
		return true, true
	case 0:
		return false, true
	}
	return false, false
}

// canTellSandboxed is whether processSandboxed can answer in this build.
const canTellSandboxed = true
