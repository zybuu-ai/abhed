//go:build unix

package bgcli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/zybuu-ai/abhed/bgjob"
)

// readNoFollow reads the file at rel, a slash-separated name under root, by
// opening each folder on the way and then the file relative to the one
// before, none of them through a link. A link anywhere on the path, or a
// file that is not a plain one, is skipped with the reason why.
func readNoFollow(root *os.File, rel string) (data []byte, fi os.FileInfo, skip string, err error) {
	parts := strings.Split(rel, "/")
	dirfd := int(root.Fd())
	var opened []int
	defer func() {
		for _, fd := range opened {
			_ = unix.Close(fd)
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		fd, err := unix.Openat(dirfd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		switch {
		case errors.Is(err, unix.ENOENT):
			return nil, nil, "", os.ErrNotExist
		case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
			return nil, nil, "reached through a link", nil
		case err != nil:
			return nil, nil, "", fmt.Errorf("%s: %w", rel, err)
		}
		opened = append(opened, fd)
		dirfd = fd
	}
	fd, err := unix.Openat(dirfd, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, nil, "", os.ErrNotExist
	case errors.Is(err, unix.ELOOP):
		return nil, nil, "a link", nil
	case err != nil:
		return nil, nil, "", fmt.Errorf("%s: %w", rel, err)
	}
	f := os.NewFile(uintptr(fd), rel)
	defer func() { _ = f.Close() }()
	if fi, err = f.Stat(); err != nil {
		return nil, nil, "", err
	}
	switch {
	case !fi.Mode().IsRegular():
		return nil, nil, "not a regular file now", nil
	case fi.Size() > bgjob.MaxSnapshotFile:
		return nil, nil, fmt.Sprintf("over %d MiB", bgjob.MaxSnapshotFile>>20), nil
	}
	data, err = io.ReadAll(io.LimitReader(f, bgjob.MaxSnapshotFile+1))
	if err != nil {
		return nil, nil, "", err
	}
	if len(data) > bgjob.MaxSnapshotFile {
		return nil, nil, fmt.Sprintf("over %d MiB", bgjob.MaxSnapshotFile>>20), nil
	}
	return data, fi, "", nil
}
