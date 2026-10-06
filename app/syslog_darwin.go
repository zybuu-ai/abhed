package app

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// writeSysLog sends msg to the unified log through /usr/bin/logger, named by
// its absolute path so PATH cannot substitute another. Only an administrator
// can erase the unified log.
func writeSysLog(msg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/logger", "-p", "auth.notice", "-t", sysLogTag, msg).CombinedOutput() // #nosec G204 -- a fixed program; msg is one escaped argument
	if err != nil {
		return fmt.Errorf("/usr/bin/logger: %w %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
