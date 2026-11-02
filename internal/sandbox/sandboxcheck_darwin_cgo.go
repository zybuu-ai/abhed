//go:build darwin && cgo

package sandbox

/*
#include <stdlib.h>
#include <sys/types.h>
extern int sandbox_check(pid_t pid, const char *operation, int type, ...);
static int abhed_path_denied(pid_t pid, const char *path) { return sandbox_check(pid, "file-read-data", 1, path); }
*/
import "C"

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// pathDenied reports whether pid's sandbox profile denies reading path
// (SANDBOX_FILTER_PATH); known is false where that cannot be told. A path
// that does not exist reads as denied, so the paths asked about must exist.
func pathDenied(pid int, path string) (denied, known bool) {
	if pid <= 1 {
		return false, false
	}
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	switch C.abhed_path_denied(C.pid_t(pid), cp) {
	case 1:
		return true, true
	case 0:
		return false, true
	}
	return false, false
}

// userPIDs are this real user's live processes.
func userPIDs() []int {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.ruid", os.Getuid())
	if err != nil {
		return nil
	}
	out := make([]int, 0, len(procs))
	for _, p := range procs {
		if pid := int(p.Proc.P_pid); pid > 1 && p.Proc.P_stat != 5 {
			out = append(out, pid)
		}
	}
	return out
}

// canTellSandboxed is whether pathDenied can answer in this build.
const canTellSandboxed = true
