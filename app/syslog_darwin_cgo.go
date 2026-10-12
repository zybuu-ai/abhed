//go:build darwin && cgo

package app

/*
#include <os/log.h>
#include <pthread.h>
#include <stdlib.h>

static os_log_t abhed_admin_log;
static pthread_once_t abhed_admin_once = PTHREAD_ONCE_INIT;
static int abhed_admin_creates;

// Made once, whichever goroutine's thread logs first.
static void abhed_admin_init(void) {
	abhed_admin_log = os_log_create("com.zybuu.abhed", "admin");
	abhed_admin_creates++;
}

// abhed_admin_write logs one entry, public so it is not redacted as <private>.
static void abhed_admin_write(const char *msg) {
	pthread_once(&abhed_admin_once, abhed_admin_init);
	os_log_with_type(abhed_admin_log, OS_LOG_TYPE_DEFAULT, "%{public}s", msg);
}

static int abhed_admin_created(void) {
	pthread_once(&abhed_admin_once, abhed_admin_init);
	return abhed_admin_creates;
}
*/
import "C"

import "unsafe"

// sysLogBackend names the writer this build uses.
const sysLogBackend = "os_log"

// sysLogSubsystem and sysLogCategory are where the entries are in the unified log.
const (
	sysLogSubsystem = "com.zybuu.abhed"
	sysLogCategory  = "admin"
)

// writeSysLog writes msg to the unified log through os_log, so the system
// records abhed itself, by its process and image path, as the sender.
func writeSysLog(msg string) error {
	c := C.CString(msg)
	defer C.free(unsafe.Pointer(c))
	C.abhed_admin_write(c)
	return nil
}

// sysLogHandles is how many os_log handles were made: one, however many
// goroutines logged at once.
func sysLogHandles() int { return int(C.abhed_admin_created()) }
