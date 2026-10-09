package microvm

import (
	"context"
	"fmt"
	"os"
	"time"
)

func probeHelper(path string) error {
	if path == "" {
		return ErrNoHelper
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w (%s: %w)", ErrNoHelper, path, err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must be an executable file writable only by its owner", path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return runHelperCheck(ctx, path)
}
