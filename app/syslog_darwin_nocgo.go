//go:build darwin && !cgo

package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// sysLogBackend names the writer this build uses.
const sysLogBackend = "logger"

var warnLogger sync.Once

// writeSysLog sends msg to the unified log through /usr/bin/logger, named by
// its absolute path so PATH cannot substitute another. A build without cgo
// cannot call os_log itself, so the entry's sender is logger, not abhed.
func writeSysLog(msg string) error {
	warnLogger.Do(func() {
		fmt.Fprintln(os.Stderr, "abhed: warning: this build writes the system log through /usr/bin/logger, "+
			"so its entries are not attributable to abhed; admin.jsonl is the record")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/logger", "-p", "auth.notice", "-t", sysLogTag, msg).CombinedOutput() // #nosec G204 -- a fixed program; msg is one escaped argument
	if err != nil {
		return fmt.Errorf("/usr/bin/logger: %w %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
