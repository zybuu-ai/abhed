package microvm

import (
	"context"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// dialUnix connects to the socket at path without following a symlink in
// its last element. The VMM, which runs as another user, owns the folder the
// socket is in and could otherwise point it at any socket the driver can
// reach; the folders above it belong to the driver.
func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return nil, fmt.Errorf("%s is not a socket", path)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d", fd))
}
