package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The sha256 of each abhed-helper the release build ships beside the
// engine, set with -ldflags -X; empty in a build that ships none.
var (
	helperSHA256LinuxAMD64 string
	helperSHA256LinuxARM64 string
	helperSHA256LinuxS390X string
)

// EngineOnAnotherHost reports whether DOCKER_HOST or CONTAINER_HOST names an
// engine on another machine (tcp://, or ssh:// to a host not this one),
// whose file system does not hold the helper's local copy. A unix socket,
// such as a Podman machine's, is this machine's VM.
func EngineOnAnotherHost() bool {
	for _, k := range []string{"DOCKER_HOST", "CONTAINER_HOST"} {
		v := os.Getenv(k)
		if v == "" || strings.HasPrefix(v, "unix://") || strings.HasPrefix(v, "npipe://") {
			continue
		}
		u, err := url.Parse(v)
		if err != nil {
			return true
		}
		h := u.Hostname()
		if h != "localhost" && h != "127.0.0.1" && h != "::1" {
			return true
		}
	}
	return false
}

// RemoteHelperRefusal is why the shipped helper is not used for an engine
// on another host, and what to do.
const RemoteHelperRefusal = "the container engine is on another host (DOCKER_HOST or CONTAINER_HOST), where the Linux helper shipped " +
	"with Abhed is not, so it is not used as the egress relay there; have your administrator set control_plane.<engine>.relay_binary " +
	"to a Linux build of Abhed or abhed-helper on that host, or use a local engine"

// HelperName is the helper's file name beside the engine for arch.
func HelperName(arch string) string { return "abhed-helper-linux-" + arch }

// helperPin is the pinned checksum for arch; tests set it.
var helperPin = func(arch string) string {
	return map[string]string{"amd64": helperSHA256LinuxAMD64, "arm64": helperSHA256LinuxARM64, "s390x": helperSHA256LinuxS390X}[arch]
}

// helperBeside finds the helper shipped with this binary: beside it, in a
// macOS app's Resources, or in ../lib/abhed; tests set it. Where it is found
// does not matter, since only a file matching the pin is used.
var helperBeside = func(arch string) (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	candidates := []string{dir, filepath.Join(dir, "..", "Resources"), filepath.Join(dir, "..", "lib", "abhed")}
	for _, c := range candidates {
		if info, err := os.Lstat(filepath.Join(c, HelperName(arch))); err == nil && info.Mode().IsRegular() {
			return filepath.Join(c, HelperName(arch)), nil
		}
	}
	return filepath.Join(dir, HelperName(arch)), nil
}

// helperCacheRoot is where verified copies are kept; tests set it.
var helperCacheRoot = func() (string, error) {
	dir, err := os.UserCacheDir()
	return filepath.Join(dir, "abhed", "helper"), err
}

var helperMu sync.Mutex

// BundledHelper is hashed in full on every call, each time before it is
// bound into a container, so a copy changed since, even with its size and
// time put back, is replaced from the shipped file before it is used.
//
// BundledHelper is the Linux helper shipped beside this engine for arch,
// when this build pins its checksum: a copy under the user's cache folder,
// which a container engine in a VM can see, checked against the pin. A file
// that does not match is never used. "" and why when there is none.
func BundledHelper(arch string) (string, string) {
	want := helperPin(arch)
	if want == "" {
		return "", "this build of Abhed ships no Linux helper for " + arch
	}
	root, err := helperCacheRoot()
	if err != nil {
		return "", "finding a cache folder for the Linux helper: " + err.Error()
	}
	helperMu.Lock()
	defer helperMu.Unlock()
	dst := filepath.Join(root, want, HelperName(arch))
	if ok, _ := matches(dst, want); ok {
		return dst, ""
	}
	src, err := helperBeside(arch)
	if err != nil {
		return "", "finding this binary: " + err.Error()
	}
	if info, err := os.Lstat(src); err != nil || !info.Mode().IsRegular() {
		return "", "the Linux helper " + src + " is not there"
	}
	if err := copyHelper(src, dst, want); err != nil {
		return "", err.Error()
	}
	return dst, ""
}

// matches reports whether the regular file at path has sha256 want.
func matches(path, want string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false, err
	}
	f, err := os.Open(path) // #nosec G304 -- the helper's own cache copy
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

// copyHelper copies src to dst, hashing as it copies, and keeps the copy
// only when it matches want, so what is checked is what is mounted.
func copyHelper(src, dst, want string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { // #nosec G301 -- the engine's VM reads it
		return fmt.Errorf("making the Linux helper's cache folder: %w", err)
	}
	in, err := os.Open(src) // #nosec G304 -- the helper beside this binary
	if err != nil {
		return fmt.Errorf("the Linux helper cannot be read: %w", err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".helper-*")
	if err != nil {
		return fmt.Errorf("copying the Linux helper: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone once renamed
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), in)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("copying the Linux helper: %w", err)
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return fmt.Errorf("the Linux helper %s does not match the checksum this build pins, so it is not used", src)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { // #nosec G302 -- an executable run in the container
		return fmt.Errorf("copying the Linux helper: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("copying the Linux helper: %w", err)
	}
	if ok, err := matches(dst, want); err != nil {
		return fmt.Errorf("checking the Linux helper's copy: %w", err)
	} else if !ok {
		return fmt.Errorf("the Linux helper's copy does not match its checksum")
	}
	return nil
}
