//go:build !windows

package microvm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// buildImage copies the workspace folder src into a new ext4 image at
// dir/workspace.ext4 of sizeMiB. When as is set, mkfs runs as that user,
// so a driver running as root copies only what the user asking could read.
func buildImage(ctx context.Context, mkfs, src, dir string, sizeMiB int, as *Credential) (string, error) {
	fi, err := os.Stat(src)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("the workspace %s is not a folder", src)
	}
	bin, err := exec.LookPath(mkfs)
	if err != nil {
		return "", fmt.Errorf("copying the workspace into the VM needs mkfs.ext4 (e2fsprogs 1.43 or later): %w", err)
	}
	build := filepath.Join(dir, "build")
	_ = os.RemoveAll(build)
	if err := os.Mkdir(build, 0o700); err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(build) }()
	if as != nil {
		if err := os.Chown(build, int(as.UID), int(as.GID)); err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	tmp := filepath.Join(build, "workspace.ext4")
	// root_owner gives the image's top folder to the guest's command user.
	cmd := exec.CommandContext(ctx, bin, "-q", "-F", "-t", "ext4", "-L", "workspace",
		"-E", "root_owner=1000:1000", "-d", src, tmp, fmt.Sprintf("%dM", sizeMiB))
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	if as != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: as.UID, Gid: as.GID}}
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building the workspace image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	img := filepath.Join(dir, "workspace.ext4")
	if err := os.Rename(tmp, img); err != nil {
		return "", err
	}
	// The build folder was the user's; what was moved out of it is checked
	// only now, in a folder they cannot reach.
	if fi, err := os.Lstat(img); err != nil || !fi.Mode().IsRegular() {
		_ = os.Remove(img)
		return "", fmt.Errorf("the workspace image is not a regular file")
	}
	if as != nil {
		if err := os.Lchown(img, 0, 0); err != nil {
			return "", err
		}
	}
	return img, os.Chmod(img, 0o600)
}
