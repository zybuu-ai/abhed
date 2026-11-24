//go:build unix

package hostgit

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// moveInto moves the folder staged to ws/parts..., opening each folder on
// the way without following a link, so a folder swapped for a link since it
// was checked is refused, not followed; none on the way may be a git folder.
func moveInto(ws string, parts []string, staged string) error {
	fd, err := unix.Open(ws, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("the workspace could not be opened: %w", err)
	}
	for _, part := range parts[:len(parts)-1] {
		if strings.EqualFold(part, ".git") || part == ".." || part == "." {
			_ = unix.Close(fd)
			return fmt.Errorf("%s is not a folder a clone may go through", part)
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return fmt.Errorf("the folder %s could not be opened without following a link: %w", part, err)
		}
		fd = next
	}
	defer func() { _ = unix.Close(fd) }()
	name := parts[len(parts)-1]
	if strings.EqualFold(name, ".git") {
		return errors.New("a clone is not made as a git folder")
	}
	// The name must still be free: a link or folder made there since is not replaced.
	var st unix.Stat_t
	if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return fmt.Errorf("%s is already there; clone into a new folder", name)
	}
	if err := unix.Renameat(unix.AT_FDCWD, staged, fd, name); err != nil {
		if errors.Is(err, unix.EXDEV) {
			return errors.New("the workspace is on another disk than ~/.abhed, where the clone is made first; clone it yourself")
		}
		return fmt.Errorf("the clone could not be moved into the workspace: %w", err)
	}
	return nil
}
